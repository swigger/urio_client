package logging

import (
	"fmt"
	"io"
	"log/slog"
	"strings"
)

func Level(name string) (slog.Level, error) {
	switch strings.ToLower(name) {
	case "debug":
		return slog.LevelDebug, nil
	case "", "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("log_level must be debug, info, warn, or error")
	}
}

func Configure(w io.Writer, name string) error {
	level, err := Level(name)
	if err != nil {
		return err
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: level})))
	return nil
}
