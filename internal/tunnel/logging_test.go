package tunnel

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	old := slog.Default()
	var b bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&b, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(old) })
	return &b
}
func TestExitLoggingAndUpstreamLevels(t *testing.T) {
	b := captureLogs(t)
	l := connectLogger{secrets: []string{"test-secret"}}
	l.Infof("event=sticky_redial action=dial test-secret")
	l.V(1).Infof("debug hidden")
	if l.V(1).Enabled() {
		t.Fatal("verbose enabled at info")
	}
	state := &exitLog{}
	a, c := connect.NewId(), connect.NewId()
	first, second := connect.NewId(), connect.NewId()
	publish := func(id, provider connect.Id, s connect.ProviderState) {
		state.update(nil, map[connect.Id]*connect.ProviderEvent{id: {ClientId: id, EgressClientId: provider, State: s, EventTime: time.Now()}}, false)
	}
	publish(a, first, connect.ProviderStateAdded)
	publish(a, first, connect.ProviderStateAdded)
	publish(a, first, connect.ProviderStateRemoved)
	publish(c, first, connect.ProviderStateAdded)
	publish(c, first, connect.ProviderStateRemoved)
	publish(connect.NewId(), second, connect.ProviderStateAdded)
	text := b.String()
	for _, want := range []string{"sticky_redial", "[redacted]", "[EXIT] selected", "reconnected same provider", "switched provider after failure"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing log: %s", want)
		}
	}
	if strings.Contains(text, "test-secret") || strings.Contains(text, "debug hidden") || strings.Count(text, "[EXIT] selected") != 1 {
		t.Fatal(text)
	}
}
func TestPacketLogHandshakeExpiry(t *testing.T) {
	b := captureLogs(t)
	now := time.Now()
	key := flowKey{"tcp", "192.168.137.2:1234", "203.0.113.1:443"}
	l := packetLog{flows: map[flowKey]*flowLog{key: {native: true, handshake: true, started: now.Add(-time.Minute), seen: now}}}
	l.sweep(now, false)
	l.sweep(now, false)
	if strings.Count(b.String(), "TCP handshake") != 1 {
		t.Fatal(b.String())
	}
}
