package proxy

import (
	"bytes"
	"context"
	"io"
	"net"
	"strconv"
	"sync"
	"time"
)

func udpAssociate(ctx context.Context, c net.Conn, r io.Reader, dial DialFunc, requested string) {
	local := c.LocalAddr().(*net.TCPAddr)
	peer := c.RemoteAddr().(*net.TCPAddr)
	u, err := net.ListenUDP("udp", &net.UDPAddr{IP: local.IP})
	if err != nil {
		reply(c, 1, nil)
		return
	}
	defer u.Close()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { u.Close() })
	defer stop()
	reply(c, 0, u.LocalAddr().(*net.UDPAddr))
	c.SetDeadline(time.Time{})
	go func() { io.Copy(io.Discard, r); cancel() }()
	var pinned *net.UDPAddr
	if _, port, err := net.SplitHostPort(requested); err == nil {
		p, _ := strconv.Atoi(port)
		if p != 0 {
			pinned = &net.UDPAddr{IP: peer.IP, Port: p}
		}
	}
	var mu sync.Mutex
	flows := map[string]net.Conn{}
	var wg sync.WaitGroup
	defer func() {
		mu.Lock()
		for _, f := range flows {
			f.Close()
		}
		mu.Unlock()
		wg.Wait()
	}()
	b := make([]byte, 65535)
	for {
		n, src, err := u.ReadFromUDP(b)
		if err != nil {
			return
		}
		if !src.IP.Equal(peer.IP) || n < 4 || b[0] != 0 || b[1] != 0 || b[2] != 0 {
			continue
		}
		if pinned == nil {
			pinned = src
		}
		if src.Port != pinned.Port {
			continue
		}
		reader := bytes.NewReader(b[3:n])
		address, err := readAddress(reader)
		if err != nil {
			continue
		}
		payload := b[n-reader.Len() : n]
		mu.Lock()
		f := flows[address]
		full := len(flows) >= 128
		mu.Unlock()
		if f == nil {
			if full {
				continue
			}
			f, err = boundedDial(ctx, dial, "udp", address)
			if err != nil {
				continue
			}
			mu.Lock()
			flows[address] = f
			mu.Unlock()
			wg.Add(1)
			go func(address string, flow net.Conn, dst *net.UDPAddr) {
				defer wg.Done()
				defer flow.Close()
				defer func() { mu.Lock(); delete(flows, address); mu.Unlock() }()
				stop := context.AfterFunc(ctx, func() { flow.Close() })
				defer stop()
				buf := make([]byte, 65507)
				for {
					flow.SetReadDeadline(time.Now().Add(2 * time.Minute))
					n, err := flow.Read(buf)
					if err != nil {
						return
					}
					remote, ok := flow.RemoteAddr().(*net.UDPAddr)
					if !ok {
						remote, err = net.ResolveUDPAddr("udp", flow.RemoteAddr().String())
						if err != nil {
							return
						}
					}
					packet := append([]byte{0, 0, 0}, addressBytes(remote)...)
					packet = append(packet, buf[:n]...)
					if _, err = u.WriteToUDP(packet, dst); err != nil {
						return
					}
				}
			}(address, f, pinned)
		}
		f.SetWriteDeadline(time.Now().Add(15 * time.Second))
		f.Write(payload)
	}
}
