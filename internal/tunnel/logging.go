package tunnel

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sort"
	"strings"
	"sync"

	"github.com/urnetwork/connect"
)

// Upstream Info contains reliability events; its V levels are debug traces.
type connectLogger struct{ secrets []string }

func (l connectLogger) line(level slog.Level, s string) {
	if level == slog.LevelInfo && (strings.Contains(s, "event=probe_sweep ") || strings.Contains(s, "event=probe ") && strings.Contains(s, "result=pass ") || strings.Contains(s, "event=probe_sweep_result ") && strings.Contains(s, "failed=0 ")) {
		level = slog.LevelDebug
	}
	for _, secret := range l.secrets {
		if secret != "" {
			s = strings.ReplaceAll(s, secret, "[redacted]")
		}
	}
	slog.Log(context.Background(), level, strings.TrimSpace(s), "component", "urnetwork")
}
func (l connectLogger) Info(a ...any)               { l.line(slog.LevelInfo, fmt.Sprint(a...)) }
func (l connectLogger) Infof(f string, a ...any)    { l.line(slog.LevelInfo, fmt.Sprintf(f, a...)) }
func (l connectLogger) Warningf(f string, a ...any) { l.line(slog.LevelWarn, fmt.Sprintf(f, a...)) }
func (l connectLogger) Errorf(f string, a ...any)   { l.line(slog.LevelError, fmt.Sprintf(f, a...)) }
func (l connectLogger) V(_ int32) connect.Verbose   { return connectVerbose{l} }

type connectVerbose struct{ connectLogger }

func (l connectVerbose) Enabled() bool {
	return slog.Default().Enabled(context.Background(), slog.LevelDebug)
}
func (l connectVerbose) Info(a ...any) {
	if l.Enabled() {
		l.line(slog.LevelDebug, fmt.Sprint(a...))
	}
}
func (l connectVerbose) Infof(f string, a ...any) {
	if l.Enabled() {
		l.line(slog.LevelDebug, fmt.Sprintf(f, a...))
	}
}

type exitLog struct {
	mu       sync.Mutex
	states   map[connect.Id]connect.ProviderState
	last     connect.Id
	selected bool
	reason   string
}

func (l *exitLog) update(w *connect.WindowExpandEvent, events map[connect.Id]*connect.ProviderEvent, reset bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.states == nil {
		l.states = make(map[connect.Id]connect.ProviderState)
	}
	ordered := make([]*connect.ProviderEvent, 0, len(events))
	for _, e := range events {
		ordered = append(ordered, e)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].EventTime.Before(ordered[j].EventTime) })
	for _, e := range ordered {
		if l.states[e.ClientId] == e.State {
			continue
		}
		switch e.State {
		case connect.ProviderStateAdded:
			switch {
			case !l.selected:
				slog.Info("[EXIT] selected", "provider", e.EgressClientId)
			case l.last == e.EgressClientId:
				slog.Info("[EXIT] reconnected same provider", "provider", e.EgressClientId)
			default:
				slog.Warn("[EXIT] switched provider after failure", "previous", l.last, "provider", e.EgressClientId)
			}
			l.last, l.selected = e.EgressClientId, true
		case connect.ProviderStateRemoved:
			slog.Warn("[EXIT] provider removed; recovering", "provider", e.EgressClientId)
		case connect.ProviderStateEvaluationFailed:
			slog.Warn("[EXIT] provider evaluation failed", "provider", e.EgressClientId)
		case connect.ProviderStateInEvaluation:
			slog.Info("[EXIT] evaluating provider", "provider", e.EgressClientId)
		}
		if e.State.IsTerminal() {
			delete(l.states, e.ClientId)
		} else {
			l.states[e.ClientId] = e.State
		}
	}
	if reset {
		for id := range l.states {
			if _, ok := events[id]; !ok {
				delete(l.states, id)
			}
		}
	}
	if w != nil {
		reason := ""
		if !w.MinSatisfied {
			reason = fmt.Sprintf("%s (failed=%v)", w.Reason, w.Failed)
		}
		if reason != l.reason && reason != "" {
			slog.Warn("[EXIT] waiting for usable provider", "reason", reason)
		}
		l.reason = reason
	}
}

// Preserve the upstream resolver's family race and cache behavior. The observer
// exposes progress without issuing a second lookup just for logging.
func (c *Client) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, _, _ := net.SplitHostPort(address)
	var mu sync.Mutex
	started, answered := false, false
	if host != "" && net.ParseIP(host) == nil {
		started = true
		slog.Info("[DNS] resolving", "name", host, "via", "urio")
	}
	ctx = connect.WithTunDialObserver(ctx, func(e connect.TunDialEvent) {
		mu.Lock()
		defer mu.Unlock()
		switch e {
		case connect.TunDialDnsStarted:
			if !started {
				started = true
				slog.Info("[DNS] resolving", "name", host, "via", "urio")
			}
		case connect.TunDialDnsAnswered:
			if !answered {
				answered = true
				slog.Info("[DNS] answered", "name", host, "via", "urio")
			}
		}
	})
	conn, err := c.Stack.DialContext(ctx, network, address)
	mu.Lock()
	defer mu.Unlock()
	if started && conn != nil && !answered {
		answered = true
		slog.Info("[DNS] answered", "name", host, "remote", conn.RemoteAddr(), "via", "urio")
	}
	if started && !answered && err != nil {
		slog.Warn("[DNS] resolution failed", "name", host, "error", err)
	}
	return conn, err
}
