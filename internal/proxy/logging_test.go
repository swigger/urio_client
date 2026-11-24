package proxy

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"strings"
	"testing"
)

func TestDialFailureLogAndLevel(t *testing.T) {
	old := slog.Default()
	defer slog.SetDefault(old)
	var b bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&b, &slog.HandlerOptions{Level: slog.LevelWarn})))
	d := loggedDial(context.Background(), func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("provider unavailable")
	}, "socks5", "127.0.0.1:1234")
	_, err := d(context.Background(), "tcp", "example.com:443")
	if err == nil {
		t.Fatal("dial failure hidden")
	}
	text := b.String()
	if !strings.Contains(text, "[CONN] failed") || !strings.Contains(text, "example.com:443") || strings.Contains(text, "connecting") {
		t.Fatal(text)
	}
}
