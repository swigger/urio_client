//go:build linux

package tunnel

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/swigger/urio-client/internal/config"
)

// Run ONLY in an isolated network namespace:
// sudo unshare -n sh -c 'ip link set lo up; URIO_TEST_TUN=1 ./tunnel.test -test.run TestNative'
func TestNativePacketAndRoutes(t *testing.T) {
	if os.Getenv("URIO_TEST_TUN") != "1" {
		t.Skip("requires isolated privileged network namespace")
	}
	routes := func() []byte {
		b, err := exec.Command("ip", "-4", "route", "show", "table", "all").Output()
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	before := routes()
	cfg := config.Defaults().TUN
	cfg.Name = "urio-test"
	cfg.AutoRoute = true
	n, err := OpenNative(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	if bytes.Equal(before, routes()) {
		t.Fatal("auto_route did not install routes")
	}
	c, err := net.DialUDP("udp4", nil, &net.UDPAddr{IP: net.IPv4(203, 0, 113, 9), Port: 53})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	done := make(chan error, 1)
	go func() {
		buf := make([]byte, 2048)
		var p []byte
		for {
			size, err := n.ReadPacket(buf)
			if err != nil {
				done <- err
				return
			}
			p = buf[:size]
			// Linux emits an IPv6 router solicitation when a link comes up.
			if len(p) > 0 && p[0]>>4 == 4 {
				break
			}
		}
		if len(p) < 32 || p[0] != 0x45 || p[9] != 17 || string(p[28:]) != "ping" {
			done <- fmt.Errorf("unexpected packet: %x", p)
			return
		}
		src := append([]byte{}, p[12:16]...)
		copy(p[12:16], p[16:20])
		copy(p[16:20], src)
		sp := binary.BigEndian.Uint16(p[20:22])
		copy(p[20:22], p[22:24])
		binary.BigEndian.PutUint16(p[22:24], sp)
		p[26], p[27] = 0, 0
		copy(p[28:], "pong")
		p[10], p[11] = 0, 0
		sum := uint32(0)
		for i := 0; i < 20; i += 2 {
			sum += uint32(binary.BigEndian.Uint16(p[i : i+2]))
		}
		for sum > 65535 {
			sum = (sum & 65535) + (sum >> 16)
		}
		binary.BigEndian.PutUint16(p[10:12], ^uint16(sum))
		n.WritePacket(p)
		done <- nil
	}()
	if _, err = c.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 64)
	size, err := c.Read(b)
	if err != nil || string(b[:size]) != "pong" {
		select {
		case detail := <-done:
			t.Logf("packet result: %v", detail)
		default:
			t.Log("no TUN packet read")
		}
		select {
		case detail := <-n.Errors:
			t.Logf("TUN write result: %v", detail)
		default:
		}
		t.Logf("routes: %s", routes())
		t.Fatalf("TUN roundtrip: %q, %v", b[:size], err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	n.Close()
	if after := routes(); !bytes.Equal(before, after) {
		t.Fatalf("routes not restored\nbefore: %s\nafter: %s", before, after)
	}
	cfg.AutoRoute = false
	n, err = OpenNative(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	b = routes()
	if bytes.Contains(b, []byte("default dev urio-test")) {
		t.Fatalf("default route changed without opt-in: %s", b)
	}
}
