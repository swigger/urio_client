package tunnel

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"net/netip"
	"runtime"
	"strings"
	"sync"
	"time"

	tun "github.com/metacubex/sing-tun"
	"github.com/metacubex/sing/common/control"
	"github.com/metacubex/sing/common/logger"
	"github.com/swigger/urio-client/internal/config"
)

func equal(a, b string) bool { return strings.EqualFold(a, b) }

type Native struct {
	dev    tun.Tun
	Errors chan error
	mu     sync.Mutex
	once   sync.Once
}

func OpenNative(c config.TUN) (*Native, error) {
	opts := tun.Options{Name: c.Name, Inet4Address: []netip.Prefix{netip.MustParsePrefix(c.Address)}, MTU: uint32(c.MTU), AutoRoute: c.AutoRoute, IPRoute2TableIndex: 2022, IPRoute2RuleIndex: 9000, Logger: logger.NOP(), EXP_DisableDNSHijack: true}
	if runtime.GOOS == "darwin" && opts.Name == "utun" {
		opts.Name = tun.CalculateInterfaceName("utun")
	}
	dev, err := tun.New(opts)
	if err != nil {
		return nil, fmt.Errorf("create TUN (administrator/root privileges required): %w", err)
	}
	return &Native{dev: dev, Errors: make(chan error, 1)}, nil
}
func (n *Native) ReadPacket(b []byte) (int, error) {
	if runtime.GOOS == "darwin" {
		tmp := make([]byte, len(b)+4)
		size, err := n.dev.Read(tmp)
		if err != nil {
			return 0, err
		}
		if size < 4 {
			return 0, fmt.Errorf("short utun packet")
		}
		return copy(b, tmp[4:size]), nil
	}
	return n.dev.Read(b)
}
func (n *Native) WritePacket(p []byte) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if runtime.GOOS == "darwin" {
		b := make([]byte, 4+len(p))
		family := uint32(2)
		if p[0]>>4 == 6 {
			family = 30
		}
		binary.BigEndian.PutUint32(b, family)
		copy(b[4:], p)
		p = b
	}
	if _, err := n.dev.Write(p); err != nil {
		n.Report(err)
	}
}
func (n *Native) Report(err error) {
	select {
	case n.Errors <- err:
	default:
	}
}
func (n *Native) Close() { n.once.Do(func() { n.dev.Close() }) }

func Underlay(interfaceName string) (func(context.Context, string, string) (net.Conn, error), error) {
	d := &net.Dialer{Timeout: 20 * time.Second, KeepAlive: 30 * time.Second}
	if interfaceName != "" {
		i, err := net.InterfaceByName(interfaceName)
		if err != nil {
			return nil, fmt.Errorf("uplink interface: %w", err)
		}
		d.Control = control.BindToInterface(control.NewDefaultInterfaceFinder(), i.Name, i.Index)
		// Resolve underlay hostnames over the same physical interface when routes
		// point at the TUN. This avoids bootstrap recursion on API reconnect.
		rd := &net.Dialer{Timeout: 10 * time.Second, Control: d.Control}
		d.Resolver = &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			return rd.DialContext(ctx, network, address)
		}}
	}
	return d.DialContext, nil
}
