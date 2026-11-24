package proxy

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	xproxy "golang.org/x/net/proxy"
)

func start(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, ln, (&net.Dialer{}).DialContext) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("proxy shutdown timed out")
		}
	})
	return ln.Addr().String()
}
func TestMixedHTTPAndSOCKS(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Proxy-Authorization") != "" {
			t.Error("proxy credentials leaked")
		}
		w.Write([]byte("through proxy"))
	}))
	defer server.Close()
	address := start(t)
	u, _ := url.Parse("http://" + address)
	transport := &http.Transport{Proxy: http.ProxyURL(u)}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	res, err := client.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if string(b) != "through proxy" {
		t.Fatal(string(b))
	}
	d, err := xproxy.SOCKS5("tcp", address, nil, xproxy.Direct)
	if err != nil {
		t.Fatal(err)
	}
	c, err := d.Dial("tcp", strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	io.WriteString(c, "GET / HTTP/1.1\r\nHost: test\r\nConnection: close\r\n\r\n")
	res, err = http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(res.Body)
	res.Body.Close()
	if string(b) != "through proxy" {
		t.Fatal(string(b))
	}
}
func TestHTTPConnectPreservesBufferedBytes(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		io.Copy(c, c)
	}()
	c, err := net.Dial("tcp", start(t))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	io.WriteString(c, "CONNECT "+ln.Addr().String()+" HTTP/1.1\r\nHost: "+ln.Addr().String()+"\r\n\r\nearly-data")
	r := bufio.NewReader(c)
	res, err := http.ReadResponse(r, nil)
	if err != nil || res.StatusCode != 200 {
		t.Fatal(res, err)
	}
	b := make([]byte, 10)
	if _, err = io.ReadFull(r, b); err != nil || string(b) != "early-data" {
		t.Fatal(string(b), err)
	}
}
func TestSOCKSUDPAssociate(t *testing.T) {
	echo, _ := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	defer echo.Close()
	go func() {
		b := make([]byte, 1024)
		n, a, err := echo.ReadFromUDP(b)
		if err == nil {
			echo.WriteToUDP(b[:n], a)
		}
	}()
	c, err := net.Dial("tcp", start(t))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	c.Write([]byte{5, 1, 0})
	var method [2]byte
	io.ReadFull(c, method[:])
	c.Write([]byte{5, 3, 0, 1, 0, 0, 0, 0, 0, 0})
	var header [3]byte
	if _, err = io.ReadFull(c, header[:]); err != nil || header[1] != 0 {
		t.Fatal(header, err)
	}
	addr, err := readAddress(c)
	if err != nil {
		t.Fatal(err)
	}
	remote, _ := net.ResolveUDPAddr("udp", addr)
	u, err := net.DialUDP("udp", nil, remote)
	if err != nil {
		t.Fatal(err)
	}
	defer u.Close()
	u.SetDeadline(time.Now().Add(5 * time.Second))
	packet := append([]byte{0, 0, 0}, addressBytes(echo.LocalAddr().(*net.UDPAddr))...)
	packet = append(packet, []byte("udp-data")...)
	u.Write(packet)
	b := make([]byte, 1024)
	n, err := u.Read(b)
	if err != nil || string(b[n-8:n]) != "udp-data" {
		t.Fatal(n, err)
	}
}
