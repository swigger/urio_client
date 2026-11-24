package jump

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"time"
)

type DialFunc func(context.Context, string, string) (net.Conn, error)
type Dialer struct {
	Address string
	PSK     []byte
	Dial    DialFunc
}

func (d *Dialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if network != "tcp" && network != "tcp4" && network != "tcp6" {
		return nil, fmt.Errorf("jump supports TCP underlay only")
	}
	if len(address) > 512 {
		return nil, fmt.Errorf("jump destination too long")
	}
	raw, err := d.Dial(ctx, "tcp", d.Address)
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { raw.Close() })
	defer stop()
	c, err := Client(raw, d.PSK)
	if err != nil {
		raw.Close()
		return nil, err
	}
	c.SetDeadline(time.Now().Add(30 * time.Second))
	b := make([]byte, 2+len(address))
	binary.BigEndian.PutUint16(b, uint16(len(address)))
	copy(b[2:], address)
	if err = writeAll(c, b); err == nil {
		var status [1]byte
		_, err = io.ReadFull(c, status[:])
		if err == nil && status[0] != 0 {
			err = fmt.Errorf("jump server refused destination or could not connect")
		}
	}
	if err != nil {
		c.Close()
		return nil, err
	}
	c.SetDeadline(time.Time{})
	if ctx.Err() != nil {
		c.Close()
		return nil, ctx.Err()
	}
	return c, nil
}

// Relay preserves half-close, allowing servers to respond after the client EOF.
func Relay(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	copyOne := func(dst, src net.Conn) {
		defer wg.Done()
		_, err := io.Copy(dst, src)
		if err != nil {
			a.Close()
			b.Close()
			return
		}
		if w, ok := dst.(interface{ CloseWrite() error }); ok {
			w.CloseWrite()
		} else {
			dst.Close()
		}
	}
	go copyOne(a, b)
	go copyOne(b, a)
	wg.Wait()
	a.Close()
	b.Close()
}

type ServerOptions struct {
	PSK            []byte
	AllowPrivate   bool
	MaxConnections int
	Dial           DialFunc
}

func Serve(ctx context.Context, ln net.Listener, o ServerOptions) error {
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(ctx, func() { ln.Close() })
	defer stop()
	sem := make(chan struct{}, o.MaxConnections)
	var wg sync.WaitGroup
	defer func() { cancel(); ln.Close(); wg.Wait() }()
	for {
		raw, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		select {
		case sem <- struct{}{}:
		default:
			raw.Close()
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			defer raw.Close()
			stop := context.AfterFunc(ctx, func() { raw.Close() })
			defer stop()
			c, err := Server(raw, o.PSK)
			if err != nil {
				return
			}
			c.SetDeadline(time.Now().Add(30 * time.Second))
			var size [2]byte
			if _, err = io.ReadFull(c, size[:]); err != nil {
				return
			}
			n := binary.BigEndian.Uint16(size[:])
			if n == 0 || n > 512 {
				return
			}
			b := make([]byte, n)
			if _, err = io.ReadFull(c, b); err != nil {
				return
			}
			dialCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
			defer cancel()
			up, err := dialDestination(dialCtx, string(b), o)
			if err != nil {
				c.Write([]byte{1})
				return
			}
			defer up.Close()
			stopUp := context.AfterFunc(ctx, func() { up.Close() })
			defer stopUp()
			if _, err = c.Write([]byte{0}); err != nil {
				return
			}
			c.SetDeadline(time.Time{})
			Relay(c, up)
		}()
	}
}
func dialDestination(ctx context.Context, address string, o ServerOptions) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	p, err := strconv.Atoi(port)
	if err != nil || p < 1 || p > 65535 {
		return nil, fmt.Errorf("invalid port")
	}
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	for _, ip := range addrs {
		if !o.AllowPrivate && !publicIP(ip) {
			continue
		}
		c, e := o.Dial(ctx, "tcp", net.JoinHostPort(ip.String(), port))
		if e == nil {
			return c, nil
		}
		err = e
	}
	if err == nil {
		err = fmt.Errorf("no permitted address")
	}
	return nil, err
}
func publicIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
		return false
	}
	for _, s := range []string{"100.64.0.0/10", "192.0.0.0/24", "198.18.0.0/15"} {
		if netip.MustParsePrefix(s).Contains(ip) {
			return false
		}
	}
	return true
}
