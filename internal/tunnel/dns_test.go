package tunnel

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/swigger/urio-client/internal/config"
	"github.com/urnetwork/connect"
	"golang.org/x/net/dns/dnsmessage"
)

func dnsQuestion(t *testing.T) []byte {
	t.Helper()
	m := dnsmessage.Message{Header: dnsmessage.Header{ID: 1234, RecursionDesired: true}, Questions: []dnsmessage.Question{{Name: dnsmessage.MustNewName("test.example."), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}}}
	b, err := m.Pack()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

type dnsRoundTripper func(*http.Request) (*http.Response, error)

func (f dnsRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestDNSHTTPSForUDPAndTCP(t *testing.T) {
	d := &tunnelDNS{ctx: context.Background(), virtual: netip.MustParseAddr("172.31.255.2"), upstream: "https://1.1.1.1/dns-query"}
	calls := 0
	d.http = &http.Client{Transport: dnsRoundTripper(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Method != "POST" || r.URL.String() != d.upstream || r.Header.Get("Content-Type") != "application/dns-message" {
			t.Fatal("invalid DoH request")
		}
		q, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/dns-message"}}, Body: io.NopCloser(bytes.NewReader(dnsAnswer(q)))}, nil
	})}
	for _, network := range []string{"udp", "tcp"} {
		var m dnsmessage.Message
		if m.Unpack(d.resolve(network, dnsQuestion(t), "test")) != nil || len(m.Answers) != 1 {
			t.Fatal("DoH answer missing")
		}
	}
	if calls != 2 {
		t.Fatal(calls)
	}
	d.http.Transport = dnsRoundTripper(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 503, Body: io.NopCloser(bytes.NewReader(nil))}, nil
	})
	var m dnsmessage.Message
	if m.Unpack(d.resolve("udp", dnsQuestion(t), "test")) != nil || m.RCode != dnsmessage.RCodeServerFailure {
		t.Fatal("HTTPS failure must not fall back to host DNS")
	}
}
func fakeDNSDial(answer func([]byte) []byte) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != "1.1.1.1:53" {
			return nil, fmt.Errorf("unexpected upstream %s", address)
		}
		client, server := net.Pipe()
		go func() {
			defer server.Close()
			server.SetDeadline(time.Now().Add(3 * time.Second))
			var q []byte
			if network == "tcp" {
				var size [2]byte
				if _, err := io.ReadFull(server, size[:]); err != nil {
					return
				}
				q = make([]byte, int(binary.BigEndian.Uint16(size[:])))
				if _, err := io.ReadFull(server, q); err != nil {
					return
				}
			} else {
				q = make([]byte, 65535)
				n, err := server.Read(q)
				if err != nil {
					return
				}
				q = q[:n]
			}
			r := answer(q)
			if network == "tcp" {
				writeDNSFrame(server, r)
			} else {
				server.Write(r)
			}
		}()
		return client, nil
	}
}
func dnsAnswer(q []byte) []byte {
	var m dnsmessage.Message
	m.Unpack(q)
	m.Response = true
	m.RecursionAvailable = true
	m.Answers = []dnsmessage.Resource{{Header: dnsmessage.ResourceHeader{Name: m.Questions[0].Name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: 60}, Body: &dnsmessage.AResource{A: [4]byte{203, 0, 113, 7}}}}
	b, _ := m.Pack()
	return b
}

// Exercise real gVisor UDP/TCP, virtual-address translation in both directions,
// and the injectable upstream dial. No host DNS socket or Internet is used.
func TestVirtualDNSRoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	newStack := func() *connect.Tun {
		s := connect.DefaultTunSettings()
		s.Log = connect.NewNoopLogger()
		stack, err := connect.CreateTun(ctx, s)
		if err != nil {
			t.Fatal(err)
		}
		return stack
	}
	client, server := newStack(), newStack()
	cfg := config.Defaults().TUN
	cfg.DNSUpstream = "1.1.1.1:53"
	d, err := newTunnelDNS(ctx, server, cfg, func(p []byte) { client.Write(p) })
	if err != nil {
		t.Fatal(err)
	}
	d.dial = fakeDNSDial(dnsAnswer)
	var wg sync.WaitGroup
	bridge := func(stack *connect.Tun, route func([]byte) bool) {
		defer wg.Done()
		for {
			p, err := stack.Read()
			if err != nil {
				return
			}
			if !route(p) {
				t.Errorf("packet escaped DNS bridge")
			}
			connect.MessagePoolReturn(p)
		}
	}
	wg.Add(2)
	go bridge(client, d.input)
	go bridge(server, d.output)
	defer func() { cancel(); d.Close(); client.Close(); server.Close(); wg.Wait() }()
	for _, network := range []string{"udp", "tcp"} {
		t.Run(network, func(t *testing.T) {
			response, err := exchangeDNS(ctx, client.DialContext, network, d.virtual.String()+":53", dnsQuestion(t))
			if err != nil {
				t.Fatal(err)
			}
			var m dnsmessage.Message
			if err := m.Unpack(response); err != nil {
				t.Fatal(err)
			}
			if m.ID != 1234 || !m.Response || len(m.Answers) != 1 || m.Answers[0].Body.(*dnsmessage.AResource).A != [4]byte{203, 0, 113, 7} {
				t.Fatalf("bad DNS answer: %+v", m)
			}
		})
	}
}

func TestDNSFailureAndTruncation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := &tunnelDNS{ctx: ctx, virtual: netip.MustParseAddr("172.31.255.2"), upstream: "1.1.1.1:53"}
	q := dnsQuestion(t)
	d.dial = func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("provider unavailable")
	}
	var m dnsmessage.Message
	if m.Unpack(d.resolve("udp", q, "test")) != nil || m.RCode != dnsmessage.RCodeServerFailure || m.ID != 1234 {
		t.Fatal("must return SERVFAIL on tunnel failure")
	}
	d.dial = fakeDNSDial(func(q []byte) []byte {
		var m dnsmessage.Message
		m.Unpack(q)
		m.Response = true
		for i := 0; i < 40; i++ {
			m.Answers = append(m.Answers, dnsmessage.Resource{Header: dnsmessage.ResourceHeader{Name: m.Questions[0].Name, Type: dnsmessage.TypeTXT, Class: dnsmessage.ClassINET}, Body: &dnsmessage.TXTResource{TXT: []string{"long answer content"}}})
		}
		b, _ := m.Pack()
		return b
	})
	response := d.resolve("udp", q, "test")
	if len(response) > 512 || m.Unpack(response) != nil || !m.Truncated {
		t.Fatal("UDP response must advertise TCP retry when over client limit")
	}
	if m.Unpack(d.resolve("tcp", q, "test")) != nil || m.Truncated || len(m.Answers) != 40 {
		t.Fatal("TCP must preserve complete answer")
	}
	d.dial = fakeDNSDial(func(q []byte) []byte { r := dnsAnswer(q); r[0] ^= 1; return r })
	if m.Unpack(d.resolve("udp", q, "test")) != nil || m.RCode != dnsmessage.RCodeServerFailure {
		t.Fatal("mismatched response accepted")
	}
	if d.resolve("udp", []byte{0, 1}, "test") != nil {
		t.Fatal("malformed query accepted")
	}
}

func TestDNSExchangeCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	a, b := net.Pipe()
	defer b.Close()
	done := make(chan error, 1)
	go func() {
		_, err := exchangeDNS(ctx, func(context.Context, string, string) (net.Conn, error) { return a, nil }, "udp", "unused", dnsQuestion(t))
		done <- err
	}()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("canceled exchange succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("canceled exchange leaked socket")
	}
}

func TestDNSStreamSegments(t *testing.T) {
	wire := dnsQuestion(t)
	frame := binary.BigEndian.AppendUint16(nil, uint16(len(wire)))
	frame = append(frame, wire...)
	s := dnsStream{}
	var seen [][]byte
	emit := func(b []byte) { seen = append(seen, append([]byte(nil), b...)) }
	s.feed(&connect.IpPath{Syn: true, SequenceNumber: 100}, nil, emit)
	s.feed(&connect.IpPath{SequenceNumber: 111}, frame[10:], emit)
	s.feed(&connect.IpPath{SequenceNumber: 101}, frame[:1], emit)
	s.feed(&connect.IpPath{SequenceNumber: 102}, frame[1:10], emit)
	s.feed(&connect.IpPath{SequenceNumber: 101}, frame, emit)
	if len(seen) != 1 || !bytes.Equal(seen[0], wire) {
		t.Fatal("segmented or retransmitted DNS was corrupted/duplicated")
	}
}
