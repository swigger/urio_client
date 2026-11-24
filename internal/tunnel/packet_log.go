package tunnel

import (
	"context"
	"encoding/binary"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/connect"
	"golang.org/x/net/dns/dnsmessage"
)

const maxLoggedFlows = 8192

type flowKey struct{ network, source, destination string }
type flowLog struct {
	seen, started     time.Time
	connected, warned bool
	native, handshake bool
	syn               uint32
	dns               [2]dnsStream
}
type packetLog struct {
	mu    sync.Mutex
	flows map[flowKey]*flowLog
	drops map[string]dropLog
}
type dropLog struct {
	last  time.Time
	count int
}

func pathKey(p *connect.IpPath, incoming bool) flowKey {
	src := net.JoinHostPort(p.SourceIp.String(), strconv.Itoa(p.SourcePort))
	dst := net.JoinHostPort(p.DestinationIp.String(), strconv.Itoa(p.DestinationPort))
	if incoming {
		src, dst = dst, src
	}
	return flowKey{p.Protocol.String(), src, dst}
}
func (l *packetLog) packet(packet []byte, incoming, native bool) {
	if !slog.Default().Enabled(context.Background(), slog.LevelWarn) {
		return
	}
	p, payload, err := connect.ParseIpPathWithPayload(packet)
	if err != nil {
		return
	}
	if p.DestinationIp.IsMulticast() || p.DestinationIp.Equal(net.IPv4bcast) {
		return
	}
	key := pathKey(p, incoming)
	dns := p.SourcePort == 53 || p.DestinationPort == 53
	if !native && !dns {
		return
	}
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.flows == nil {
		l.flows = make(map[flowKey]*flowLog)
	}
	f := l.flows[key]
	if f == nil || now.Sub(f.seen) > 2*time.Minute || (p.Syn && !p.Ack && f.syn != p.SequenceNumber) {
		if f == nil && len(l.flows) >= maxLoggedFlows {
			l.sweep(now, true)
		}
		f = &flowLog{started: now, syn: p.SequenceNumber, native: native, handshake: p.Syn && !p.Ack && !incoming}
		l.flows[key] = f
		if native && !incoming {
			slog.Info("[TUN] connection", "network", key.network, "source", key.source, "destination", key.destination)
		}
	}
	f.seen = now
	if incoming && p.Syn && p.Ack {
		f.connected = true
	}
	if p.Rst && !f.warned {
		f.warned = true
		if native {
			slog.Warn("[TUN] TCP reset", "source", key.source, "destination", key.destination, "incoming", incoming)
		}
	}
	if dns {
		if p.Protocol == connect.IpProtocolUdp {
			logDNS(payload, key.source, key.destination)
		}
		if p.Protocol == connect.IpProtocolTcp {
			direction := 0
			if incoming {
				direction = 1
			}
			f.dns[direction].feed(p, payload, func(b []byte) { logDNS(b, key.source, key.destination) })
		}
	}
	// Keep closed entries briefly to suppress duplicate FIN/RST and SYN retries.
}
func (l *packetLog) run(ctx context.Context) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			l.mu.Lock()
			l.sweep(now, false)
			l.mu.Unlock()
		}
	}
}
func (l *packetLog) sweep(now time.Time, pressure bool) {
	var oldest flowKey
	var oldestTime time.Time
	for k, f := range l.flows {
		if f.native && f.handshake && !f.connected && !f.warned && now.Sub(f.started) > 45*time.Second {
			f.warned = true
			slog.Warn("[TUN] TCP handshake not observed within 45s", "source", k.source, "destination", k.destination)
		}
		if now.Sub(f.seen) > 2*time.Minute {
			delete(l.flows, k)
			continue
		}
		if oldestTime.IsZero() || f.seen.Before(oldestTime) {
			oldest, oldestTime = k, f.seen
		}
	}
	if pressure && len(l.flows) >= maxLoggedFlows {
		delete(l.flows, oldest)
	}
}

func logDNS(b []byte, source, destination string) {
	var m dnsmessage.Message
	if m.Unpack(b) != nil || len(m.Questions) == 0 {
		return
	}
	for _, q := range m.Questions {
		attrs := []any{"source", source, "destination", destination, "id", m.ID, "name", q.Name.String(), "type", strings.TrimPrefix(q.Type.String(), "Type")}
		if !m.Response {
			slog.Info("[DNS] query", attrs...)
			continue
		}
		var answers []string
		for _, r := range m.Answers {
			switch v := r.Body.(type) {
			case *dnsmessage.AResource:
				answers = append(answers, net.IP(v.A[:]).String())
			case *dnsmessage.AAAAResource:
				answers = append(answers, net.IP(v.AAAA[:]).String())
			case *dnsmessage.CNAMEResource:
				answers = append(answers, v.CNAME.String())
			}
		}
		attrs = append(attrs, "rcode", strings.TrimPrefix(m.RCode.String(), "RCode"), "answers", strings.Join(answers, ","))
		if m.RCode != dnsmessage.RCodeSuccess {
			slog.Warn("[DNS] response", attrs...)
		} else {
			slog.Info("[DNS] response", attrs...)
		}
	}
}

// A bounded observer only; it never alters delivery. Contiguous TCP DNS frames
// are reassembled; retransmissions are ignored and gaps held until filled.
type dnsStream struct {
	initialized  bool
	next         uint32
	buf          []byte
	pending      map[uint32][]byte
	pendingBytes int
}

func (s *dnsStream) feed(p *connect.IpPath, b []byte, emit func([]byte)) {
	seq := p.SequenceNumber
	if p.Syn {
		seq++
		if !s.initialized {
			s.initialized = true
			s.next = seq
		}
	}
	if len(b) == 0 {
		return
	}
	if !s.initialized {
		s.initialized = true
		s.next = seq
	}
	delta := int32(seq - s.next)
	if delta < 0 {
		n := int(-int64(delta))
		if n >= len(b) {
			return
		}
		b = b[n:]
		seq = s.next
	}
	if delta > 0 {
		if s.pending == nil {
			s.pending = map[uint32][]byte{}
		}
		if _, ok := s.pending[seq]; !ok && len(s.pending) < 32 && s.pendingBytes+len(b) <= 65537 {
			s.pending[seq] = append([]byte(nil), b...)
			s.pendingBytes += len(b)
		}
		return
	}
	for {
		s.buf = append(s.buf, b...)
		s.next += uint32(len(b))
		for len(s.buf) >= 2 {
			n := int(binary.BigEndian.Uint16(s.buf))
			if len(s.buf) < n+2 {
				break
			}
			emit(s.buf[2 : n+2])
			s.buf = s.buf[n+2:]
		}
		b = s.pending[s.next]
		if b == nil {
			break
		}
		delete(s.pending, s.next)
		s.pendingBytes -= len(b)
	}
	if len(s.buf) == 0 {
		s.buf = nil
	}
}
