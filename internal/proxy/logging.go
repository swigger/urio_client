package proxy

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

func loggedDial(lifetime context.Context, dial DialFunc, inbound, source string) DialFunc {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		attrs := []any{"inbound", inbound, "network", network, "source", source, "destination", address}
		slog.Info("[CONN] connecting", attrs...)
		start := time.Now()
		conn, err := dial(ctx, network, address)
		if err != nil {
			slog.Warn("[CONN] failed", append(attrs, "error", err, "elapsed", time.Since(start))...)
			return nil, err
		}
		slog.Info("[CONN] connected", append(attrs, "remote", conn.RemoteAddr(), "elapsed", time.Since(start))...)
		return &loggedConn{Conn: conn, ctx: lifetime, attrs: attrs}, nil
	}
}

type loggedConn struct {
	net.Conn
	ctx    context.Context
	attrs  []any
	once   sync.Once
	closed atomic.Bool
}

func (c *loggedConn) failure(err error) {
	// A UDP idle deadline is normal association cleanup, not a failed dial.
	if err == nil || c.closed.Load() || errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) || c.ctx.Err() != nil {
		return
	}
	if e, ok := err.(net.Error); ok && e.Timeout() && c.Conn.RemoteAddr().Network() == "udp" {
		return
	}
	c.once.Do(func() { slog.Warn("[CONN] stream failed", append(c.attrs, "error", err)...) })
}
func (c *loggedConn) Close() error { c.closed.Store(true); return c.Conn.Close() }
func (c *loggedConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	c.failure(err)
	return n, err
}
func (c *loggedConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	c.failure(err)
	return n, err
}
func (c *loggedConn) CloseWrite() error {
	if w, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return w.CloseWrite()
	}
	return c.Conn.Close()
}
