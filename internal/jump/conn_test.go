package jump

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/netip"
	"testing"
	"time"
)

var testPSK = []byte("test-only-012345678901234567890123456789")

func pair(t *testing.T) (*Conn, *Conn) {
	t.Helper()
	a, b := net.Pipe()
	t.Cleanup(func() { a.Close(); b.Close() })
	ch := make(chan *Conn, 1)
	go func() {
		s, err := Server(b, testPSK)
		if err != nil {
			t.Error(err)
		}
		ch <- s
	}()
	c, err := Client(a, testPSK)
	if err != nil {
		t.Fatal(err)
	}
	return c, <-ch
}
func TestEncryptedStreamAndHalfClose(t *testing.T) {
	c, s := pair(t)
	payload := bytes.Repeat([]byte("payload"), 30000)
	done := make(chan error, 1)
	go func() {
		_, err := c.Write(payload)
		if err == nil {
			err = c.CloseWrite()
		}
		done <- err
	}()
	got, err := io.ReadAll(s)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("read %d bytes, %v", len(got), err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	go func() { s.Write([]byte("response after EOF")); s.CloseWrite() }()
	got, err = io.ReadAll(c)
	if err != nil || string(got) != "response after EOF" {
		t.Fatal(string(got), err)
	}
}
func TestWrongPSKRejected(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	done := make(chan error, 1)
	go func() { _, err := Server(b, testPSK); done <- err }()
	_, err := Client(a, bytes.Repeat([]byte{1}, 32))
	if err == nil {
		t.Fatal("accepted wrong PSK")
	}
	a.Close()
	if err = <-done; err == nil {
		t.Fatal("server accepted wrong PSK")
	}
}
func TestTamperedRecordRejected(t *testing.T) {
	c, s := pair(t)
	go func() { c.Conn.Write([]byte{0, 0, 0, 18}); c.Conn.Write(make([]byte, 18)) }()
	b := make([]byte, 8)
	if _, err := s.Read(b); err == nil {
		t.Fatal("accepted forged record")
	}
}
func TestReplayRecordRejected(t *testing.T) {
	c, s := pair(t)
	header := []byte{0, 0, 0, 18}
	record := append(header, c.w.Seal(nil, nonce(0), []byte{0, 'x'}, header)...)
	go func() { c.Conn.Write(record); c.Conn.Write(record) }()
	b := make([]byte, 1)
	if _, err := s.Read(b); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Read(b); err == nil {
		t.Fatal("accepted replay")
	}
}
func TestRelayEndToEndAndCancellation(t *testing.T) {
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		c, err := echo.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		io.Copy(c, c)
	}()
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	nd := &net.Dialer{}
	go func() {
		done <- Serve(ctx, ln, ServerOptions{PSK: testPSK, AllowPrivate: true, MaxConnections: 8, Dial: nd.DialContext})
	}()
	d := Dialer{Address: ln.Addr().String(), PSK: testPSK, Dial: nd.DialContext}
	c, err := d.DialContext(ctx, "tcp", echo.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	c.Write([]byte("hello"))
	b := make([]byte, 5)
	if _, err = io.ReadFull(c, b); err != nil || string(b) != "hello" {
		t.Fatal(string(b), err)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server failed to drain")
	}
}
func TestPublicDestinationPolicy(t *testing.T) {
	for _, s := range []string{"127.0.0.1", "10.0.0.1", "169.254.169.254", "100.64.1.2", "::1", "::ffff:127.0.0.1"} {
		if publicIP(netip.MustParseAddr(s)) {
			t.Fatal("private target permitted", s)
		}
	}
}
