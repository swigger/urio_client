package tunnel

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/swigger/urio-client/internal/config"
	"github.com/urnetwork/connect"
	"golang.org/x/net/dns/dnsmessage"
)

// DNS listens in the user-mode stack, never on the host's port 53. Only the
// virtual peer is translated to this listener. Upstream sockets use the same
// ur.io stack and provider as proxy/TUN traffic; there is no host dial fallback.
type tunnelDNS struct {
	virtual, local netip.Addr
	stack          *connect.Tun
	udp            net.PacketConn
	tcp            net.Listener
	upstream       string
	dial           func(context.Context, string, string) (net.Conn, error)
	write          func([]byte)
	ctx            context.Context
	cancel         context.CancelFunc
	wg             sync.WaitGroup
	sem            chan struct{}
	http           *http.Client
	transport      *http.Transport
}

func newTunnelDNS(ctx context.Context, stack *connect.Tun, c config.TUN, write func([]byte)) (*tunnelDNS, error) {
	virtual, err := c.DNSIP()
	if err != nil {
		return nil, err
	}
	local := stack.LocalAddresses()[0]
	u, err := stack.ListenUDP(&net.UDPAddr{IP: net.IP(local.AsSlice()), Port: 53})
	if err != nil {
		return nil, err
	}
	t, err := stack.ListenTCP(&net.TCPAddr{IP: net.IP(local.AsSlice()), Port: 53})
	if err != nil {
		u.Close()
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	d := &tunnelDNS{virtual: virtual, local: local, stack: stack, udp: u, tcp: t, upstream: c.DNSUpstream, dial: stack.DialContext, write: write, ctx: ctx, cancel: cancel, sem: make(chan struct{}, 128)}
	d.transport = &http.Transport{Proxy: nil, DialContext: stack.DialContext, TLSHandshakeTimeout: 10 * time.Second, IdleConnTimeout: time.Minute, ForceAttemptHTTP2: true, MaxIdleConnsPerHost: 4, MaxConnsPerHost: 16}
	d.http = &http.Client{Transport: d.transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	d.wg.Add(2)
	go d.serveUDP()
	go d.serveTCP()
	slog.Info("[DNS] TUN resolver ready", "address", net.JoinHostPort(virtual.String(), "53"), "upstream", c.DNSUpstream, "via", "urio", "protocols", "udp,tcp")
	return d, nil
}
func (d *tunnelDNS) Close() {
	d.cancel()
	d.udp.Close()
	d.tcp.Close()
	d.wg.Wait()
	d.transport.CloseIdleConnections()
}

func (d *tunnelDNS) input(p []byte) bool {
	if len(p) < 20 || p[0]>>4 != 4 || netip.AddrFrom4([4]byte(p[16:20])) != d.virtual {
		return false
	}
	b := append([]byte(nil), p...)
	if !rewriteIPv4(b, false, d.local) {
		return true
	}
	_, err := d.stack.Write(b)
	if err != nil {
		slog.Warn("[DNS] TUN input failed", "error", err)
	}
	return true
}
func (d *tunnelDNS) output(p []byte) bool {
	if len(p) < 20 || p[0]>>4 != 4 || netip.AddrFrom4([4]byte(p[12:16])) != d.local {
		return false
	}
	h := int(p[0]&15) * 4
	if h < 20 || len(p) < h+4 || (p[9] != 6 && p[9] != 17) || binary.BigEndian.Uint16(p[h:h+2]) != 53 {
		return false
	}
	if rewriteIPv4(p, true, d.virtual) {
		d.write(p)
	}
	return true
}

// Incremental pseudo-header checksum adjustment also works on first fragments;
// later fragments have only their IP header adjusted. Zero UDP checksums stay zero.
func rewriteIPv4(p []byte, source bool, to netip.Addr) bool {
	if len(p) < 20 || p[0]>>4 != 4 || !to.Is4() {
		return false
	}
	h := int(p[0]&15) * 4
	if h < 20 || len(p) < h {
		return false
	}
	offset := 16
	if source {
		offset = 12
	}
	old := [4]byte(p[offset : offset+4])
	next := to.As4()
	if binary.BigEndian.Uint16(p[6:8])&0x1fff == 0 {
		checksum := -1
		switch p[9] {
		case 6:
			checksum = h + 16
		case 17:
			checksum = h + 6
		}
		if checksum >= 0 {
			if len(p) < checksum+2 {
				return false
			}
			value := binary.BigEndian.Uint16(p[checksum : checksum+2])
			if value != 0 || p[9] == 6 {
				sum := uint32(^value)
				for i := 0; i < 4; i += 2 {
					sum += uint32(^binary.BigEndian.Uint16(old[i:i+2])) + uint32(binary.BigEndian.Uint16(next[i:i+2]))
				}
				for sum>>16 != 0 {
					sum = (sum & 65535) + (sum >> 16)
				}
				value = ^uint16(sum)
				if value == 0 && p[9] == 17 {
					value = 65535
				}
				binary.BigEndian.PutUint16(p[checksum:checksum+2], value)
			}
		}
	}
	copy(p[offset:offset+4], next[:])
	p[10], p[11] = 0, 0
	var sum uint32
	for i := 0; i < h; i += 2 {
		sum += uint32(binary.BigEndian.Uint16(p[i : i+2]))
	}
	for sum>>16 != 0 {
		sum = (sum & 65535) + (sum >> 16)
	}
	binary.BigEndian.PutUint16(p[10:12], ^uint16(sum))
	return true
}

func (d *tunnelDNS) serveUDP() {
	defer d.wg.Done()
	b := make([]byte, 65535)
	for {
		n, src, err := d.udp.ReadFrom(b)
		if err != nil {
			return
		}
		select {
		case d.sem <- struct{}{}:
		default:
			slog.Warn("[DNS] query capacity reached")
			continue
		}
		query := append([]byte(nil), b[:n]...)
		d.wg.Add(1)
		go func() {
			defer d.wg.Done()
			defer func() { <-d.sem }()
			response := d.resolve("udp", query, src.String())
			if len(response) > 0 {
				if _, err := d.udp.WriteTo(response, src); err != nil && d.ctx.Err() == nil {
					slog.Warn("[DNS] reply failed", "source", src, "error", err)
				}
			}
		}()
	}
}
func (d *tunnelDNS) serveTCP() {
	defer d.wg.Done()
	for {
		conn, err := d.tcp.Accept()
		if err != nil {
			return
		}
		select {
		case d.sem <- struct{}{}:
		default:
			conn.Close()
			slog.Warn("[DNS] TCP capacity reached")
			continue
		}
		d.wg.Add(1)
		go func() {
			defer d.wg.Done()
			defer func() { <-d.sem }()
			defer conn.Close()
			stop := context.AfterFunc(d.ctx, func() { conn.Close() })
			defer stop()
			for {
				conn.SetDeadline(time.Now().Add(30 * time.Second))
				var size [2]byte
				if _, err := io.ReadFull(conn, size[:]); err != nil {
					return
				}
				query := make([]byte, int(binary.BigEndian.Uint16(size[:])))
				if _, err := io.ReadFull(conn, query); err != nil {
					return
				}
				response := d.resolve("tcp", query, conn.RemoteAddr().String())
				if len(response) == 0 {
					return
				}
				if err := writeDNSFrame(conn, response); err != nil {
					return
				}
			}
		}()
	}
}
func (d *tunnelDNS) resolve(network string, query []byte, source string) []byte {
	var q dnsmessage.Message
	if q.Unpack(query) != nil || q.Response || len(q.Questions) != 1 {
		return nil
	}
	logDNS(query, source, d.virtual.String()+":53")
	ctx, cancel := context.WithTimeout(d.ctx, 15*time.Second)
	defer cancel()
	var response []byte
	var err error
	if strings.HasPrefix(d.upstream, "https://") {
		response, err = d.exchangeHTTPS(ctx, query)
	} else {
		response, err = exchangeDNS(ctx, d.dial, network, d.upstream, query)
	}
	if err == nil {
		var reply dnsmessage.Message
		if reply.Unpack(response) != nil || !reply.Response || reply.ID != q.ID || len(reply.Questions) != 1 || reply.Questions[0] != q.Questions[0] {
			err = fmt.Errorf("invalid upstream DNS response")
		}
	}
	if err != nil {
		slog.Warn("[DNS] upstream failed", "name", q.Questions[0].Name.String(), "upstream", d.upstream, "via", "urio", "error", err)
		response = dnsReply(q, dnsmessage.RCodeServerFailure, false)
	} else if network == "udp" {
		limit := 512
		for _, r := range q.Additionals {
			if r.Header.Type == dnsmessage.TypeOPT {
				limit = int(r.Header.Class)
				if limit < 512 {
					limit = 512
				}
				if limit > 1232 {
					limit = 1232
				}
			}
		}
		if len(response) > limit {
			response = dnsReply(q, dnsmessage.RCodeSuccess, true)
		}
	}
	logDNS(response, source, d.virtual.String()+":53")
	return response
}

func (d *tunnelDNS) exchangeHTTPS(ctx context.Context, query []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.upstream, bytes.NewReader(query))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/dns-message")
	req.Header.Set("Accept", "application/dns-message")
	res, err := d.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("DNS HTTPS upstream returned status %d", res.StatusCode)
	}
	if strings.Split(res.Header.Get("Content-Type"), ";")[0] != "application/dns-message" {
		return nil, fmt.Errorf("invalid DNS HTTPS content type")
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, 65536))
	if len(b) > 65535 {
		return nil, fmt.Errorf("oversized DNS HTTPS response")
	}
	return b, err
}
func dnsReply(q dnsmessage.Message, code dnsmessage.RCode, truncated bool) []byte {
	m := dnsmessage.Message{Header: dnsmessage.Header{ID: q.ID, Response: true, OpCode: q.OpCode, RecursionDesired: q.RecursionDesired, RecursionAvailable: true, RCode: code, Truncated: truncated}, Questions: q.Questions}
	b, _ := m.Pack()
	return b
}
func exchangeDNS(ctx context.Context, dial func(context.Context, string, string) (net.Conn, error), network, address string, query []byte) ([]byte, error) {
	c, err := dial(ctx, network, address)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()
	if deadline, ok := ctx.Deadline(); ok {
		c.SetDeadline(deadline)
	}
	if network == "tcp" {
		if err := writeDNSFrame(c, query); err != nil {
			return nil, err
		}
		var size [2]byte
		if _, err := io.ReadFull(c, size[:]); err != nil {
			return nil, err
		}
		b := make([]byte, int(binary.BigEndian.Uint16(size[:])))
		_, err := io.ReadFull(c, b)
		return b, err
	}
	if n, err := c.Write(query); err != nil {
		return nil, err
	} else if n != len(query) {
		return nil, io.ErrShortWrite
	}
	b := make([]byte, 65535)
	n, err := c.Read(b)
	return b[:n], err
}
func writeDNSFrame(w io.Writer, b []byte) error {
	frame := binary.BigEndian.AppendUint16(nil, uint16(len(b)))
	frame = append(frame, b...)
	for len(frame) > 0 {
		n, err := w.Write(frame)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		frame = frame[n:]
	}
	return nil
}
