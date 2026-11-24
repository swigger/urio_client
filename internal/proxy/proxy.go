package proxy

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/swigger/urio-client/internal/jump"
)

type DialFunc func(context.Context, string, string) (net.Conn, error)
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(b []byte) (int, error) { return c.r.Read(b) }
func (c *bufferedConn) CloseWrite() error {
	if w, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return w.CloseWrite()
	}
	return c.Conn.Close()
}

func Serve(ctx context.Context, ln net.Listener, dial DialFunc) error {
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(ctx, func() { ln.Close() })
	defer stop()
	var wg sync.WaitGroup
	defer func() { cancel(); ln.Close(); wg.Wait() }()
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer c.Close()
			stop := context.AfterFunc(ctx, func() { c.Close() })
			defer stop()
			c.SetReadDeadline(time.Now().Add(15 * time.Second))
			r := bufio.NewReaderSize(c, 32*1024)
			b, err := r.Peek(1)
			if err != nil {
				return
			}
			if b[0] == 5 {
				socks(ctx, c, r, loggedDial(ctx, dial, "socks5", c.RemoteAddr().String()))
			} else {
				httpProxy(ctx, c, r, loggedDial(ctx, dial, "http", c.RemoteAddr().String()))
			}
		}()
	}
}
func boundedDial(ctx context.Context, dial DialFunc, network, address string) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	return dial(ctx, network, address)
}
func socks(ctx context.Context, c net.Conn, r *bufio.Reader, dial DialFunc) {
	var h [2]byte
	if _, err := io.ReadFull(r, h[:]); err != nil || h[0] != 5 {
		return
	}
	m := make([]byte, h[1])
	if _, err := io.ReadFull(r, m); err != nil {
		return
	}
	ok := false
	for _, v := range m {
		if v == 0 {
			ok = true
		}
	}
	if !ok {
		c.Write([]byte{5, 255})
		return
	}
	c.Write([]byte{5, 0})
	var req [3]byte
	if _, err := io.ReadFull(r, req[:]); err != nil || req[0] != 5 || req[2] != 0 {
		return
	}
	address, err := readAddress(r)
	if err != nil {
		reply(c, 8, nil)
		return
	}
	switch req[1] {
	case 1:
		up, err := boundedDial(ctx, dial, "tcp", address)
		if err != nil {
			reply(c, 5, nil)
			return
		}
		defer up.Close()
		stop := context.AfterFunc(ctx, func() { up.Close() })
		defer stop()
		reply(c, 0, nil)
		c.SetDeadline(time.Time{})
		jump.Relay(&bufferedConn{c, r}, up)
	case 3:
		udpAssociate(ctx, c, r, dial, address)
	default:
		reply(c, 7, nil)
	}
}
func readAddress(r io.Reader) (string, error) {
	var typ [1]byte
	if _, err := io.ReadFull(r, typ[:]); err != nil {
		return "", err
	}
	var host string
	switch typ[0] {
	case 1:
		b := make([]byte, 4)
		if _, err := io.ReadFull(r, b); err != nil {
			return "", err
		}
		host = net.IP(b).String()
	case 4:
		b := make([]byte, 16)
		if _, err := io.ReadFull(r, b); err != nil {
			return "", err
		}
		host = net.IP(b).String()
	case 3:
		var n [1]byte
		if _, err := io.ReadFull(r, n[:]); err != nil || n[0] == 0 {
			return "", fmt.Errorf("bad domain")
		}
		b := make([]byte, n[0])
		if _, err := io.ReadFull(r, b); err != nil {
			return "", err
		}
		host = string(b)
	default:
		return "", fmt.Errorf("unsupported address")
	}
	var port [2]byte
	if _, err := io.ReadFull(r, port[:]); err != nil {
		return "", err
	}
	return net.JoinHostPort(host, fmt.Sprint(binary.BigEndian.Uint16(port[:]))), nil
}
func addressBytes(addr *net.UDPAddr) []byte {
	if ip := addr.IP.To4(); ip != nil {
		b := append([]byte{1}, ip...)
		return binary.BigEndian.AppendUint16(b, uint16(addr.Port))
	}
	b := append([]byte{4}, addr.IP.To16()...)
	return binary.BigEndian.AppendUint16(b, uint16(addr.Port))
}
func reply(w io.Writer, code byte, addr *net.UDPAddr) {
	if addr == nil {
		addr = &net.UDPAddr{IP: net.IPv4zero}
	}
	w.Write(append([]byte{5, code, 0}, addressBytes(addr)...))
}
func removeHopHeaders(h http.Header) {
	for _, v := range h.Values("Connection") {
		for _, name := range strings.Split(v, ",") {
			h.Del(strings.TrimSpace(name))
		}
	}
	for _, k := range []string{"Proxy-Connection", "Proxy-Authorization", "Proxy-Authenticate", "Connection", "Keep-Alive", "TE", "Trailer", "Transfer-Encoding", "Upgrade"} {
		h.Del(k)
	}
}
func httpProxy(ctx context.Context, c net.Conn, r *bufio.Reader, dial DialFunc) {
	transport := &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, n, a string) (net.Conn, error) { return boundedDial(ctx, dial, n, a) }, DisableKeepAlives: true, ResponseHeaderTimeout: 60 * time.Second}
	defer transport.CloseIdleConnections()
	for {
		req, err := http.ReadRequest(r)
		if err != nil {
			return
		}
		if req.Method == http.MethodConnect {
			addr := req.Host
			if _, _, err = net.SplitHostPort(addr); err != nil {
				io.WriteString(c, "HTTP/1.1 400 Bad Request\r\nContent-Length: 0\r\n\r\n")
				return
			}
			up, err := boundedDial(ctx, dial, "tcp", addr)
			if err != nil {
				io.WriteString(c, "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n")
				return
			}
			defer up.Close()
			stop := context.AfterFunc(ctx, func() { up.Close() })
			defer stop()
			io.WriteString(c, "HTTP/1.1 200 Connection Established\r\n\r\n")
			c.SetDeadline(time.Time{})
			jump.Relay(&bufferedConn{c, r}, up)
			return
		}
		if req.URL.Scheme != "http" || req.URL.Host == "" {
			req.Body.Close()
			io.WriteString(c, "HTTP/1.1 400 Bad Request\r\nContent-Length: 0\r\n\r\n")
			return
		}
		closeAfter := req.Close
		req.RequestURI = ""
		removeHopHeaders(req.Header)
		req = req.WithContext(ctx)
		c.SetDeadline(time.Time{})
		res, err := transport.RoundTrip(req)
		req.Body.Close()
		if err != nil {
			slog.Warn("[HTTP] request failed", "source", c.RemoteAddr(), "destination", req.URL.Host, "error", err)
			io.WriteString(c, "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n")
			return
		}
		removeHopHeaders(res.Header)
		res.Close = closeAfter
		err = res.Write(c)
		res.Body.Close()
		if err != nil || closeAfter {
			return
		}
		c.SetReadDeadline(time.Now().Add(60 * time.Second))
	}
}
