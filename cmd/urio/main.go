package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/swigger/urio-client/internal/auth"
	"github.com/swigger/urio-client/internal/config"
	"github.com/swigger/urio-client/internal/jump"
	"github.com/swigger/urio-client/internal/logging"
	"github.com/swigger/urio-client/internal/platform"
	"github.com/swigger/urio-client/internal/proxy"
	"github.com/swigger/urio-client/internal/tunnel"
	"golang.org/x/term"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "urio:", err)
		os.Exit(1)
	}
}
func run(args []string) error {
	mode := "run"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		mode = args[0]
		args = args[1:]
	}
	f := flag.NewFlagSet("urio "+mode, flag.ContinueOnError)
	path := f.String("config", "config.yaml", "YAML configuration path")
	nonInteractive := f.Bool("non-interactive", false, "never prompt; fail if credentials are unavailable or rejected")
	query := f.String("query", "", "city search text")
	jsonOutput := f.Bool("json", false, "print city list as JSON")
	selectCity := f.Bool("select", false, "interactive city selection; print configuration snippet")
	logLevel := f.String("log-level", "", "logging level: debug, info, warn, error (overrides config)")
	f.Usage = func() {
		fmt.Fprintln(f.Output(), "Usage: urio [run|cities|clear-password|jump-server|keygen|version] [options]")
		f.PrintDefaults()
	}
	if err := f.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if f.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(f.Args(), " "))
	}
	if mode == "version" {
		fmt.Println("urio-client 0.1.0")
		return nil
	}
	if mode == "keygen" {
		var b [32]byte
		if _, err := rand.Read(b[:]); err != nil {
			return err
		}
		fmt.Println(hex.EncodeToString(b[:]))
		return nil
	}
	if mode != "run" && mode != "cities" && mode != "clear-password" && mode != "jump-server" {
		return fmt.Errorf("unknown command %q", mode)
	}
	c, err := config.Load(*path)
	if err != nil {
		return err
	}
	if *logLevel != "" {
		c.LogLevel = *logLevel
	}
	if err = c.Validate(mode); err != nil {
		return err
	}
	if err = logging.Configure(os.Stderr, c.LogLevel); err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	store := auth.Keyring{Service: "urio-client:" + c.APIURL}
	if mode == "clear-password" {
		if err = store.Delete(c.Account.Email); err != nil {
			return err
		}
		fmt.Println("Saved password cleared.")
		return nil
	}
	dial, err := tunnel.Underlay(c.TUN.Interface)
	if err != nil {
		return err
	}
	if mode == "jump-server" {
		psk, err := config.Secret(c.JumpServer.PSK, c.JumpServer.PSKFile)
		if err != nil {
			return err
		}
		ln, err := net.Listen("tcp", c.JumpServer.Listen)
		if err != nil {
			return err
		}
		slog.Info("Jump server listening", "address", ln.Addr())
		return jump.Serve(ctx, ln, jump.ServerOptions{PSK: psk, AllowPrivate: c.JumpServer.AllowPrivate, MaxConnections: c.JumpServer.MaxConnections, Dial: dial})
	}
	if c.Jump.Address != "" {
		psk, err := config.Secret(c.Jump.PSK, c.Jump.PSKFile)
		if err != nil {
			return err
		}
		jd := &jump.Dialer{Address: c.Jump.Address, PSK: psk, Dial: dial}
		dial = jd.DialContext
	}
	transport := &http.Transport{Proxy: nil, DialContext: dial, TLSHandshakeTimeout: 15 * time.Second}
	defer transport.CloseIdleConnections()
	a := &platform.API{URL: c.APIURL, HTTP: &http.Client{Transport: transport, Timeout: 45 * time.Second}}
	interactive := !*nonInteractive && term.IsTerminal(int(os.Stdin.Fd()))
	token, err := auth.Login(ctx, c.Account.Email, c.Account.Password, interactive, store, a.Login, func() (string, error) {
		fmt.Fprint(os.Stderr, "ur.io password: ")
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		return string(b), err
	})
	if err != nil {
		return err
	}
	a.Token = token
	if mode == "cities" {
		cities, err := a.Cities(ctx, *query)
		if err != nil {
			return err
		}
		sort.Slice(cities, func(i, j int) bool { return cities[i].Country+cities[i].Name < cities[j].Country+cities[j].Name })
		if *jsonOutput {
			return json.NewEncoder(os.Stdout).Encode(cities)
		}
		if *selectCity {
			if !interactive {
				return fmt.Errorf("cities --select requires an interactive terminal")
			}
			return chooseCity(cities)
		}
		for _, l := range cities {
			fmt.Printf("%s\t%s\t%s\t%s\t%d providers\n", l.ID, l.Name, l.Region, l.Country, l.Count)
		}
		if len(cities) == 0 {
			return fmt.Errorf("no cities with online providers matched")
		}
		return nil
	}
	locationID, err := tunnel.ResolveCity(ctx, a, c.Exit)
	if err != nil {
		return err
	}
	// Bind the listener before installing routes so a port conflict is harmless.
	ln, err := net.Listen("tcp", c.Listen)
	if err != nil {
		return err
	}
	defer ln.Close()
	var native *tunnel.Native
	if c.TUN.Enabled {
		native, err = tunnel.OpenNative(c.TUN)
		if err != nil {
			return err
		}
		defer native.Close()
		slog.Info("[TUN] ready", "name", c.TUN.Name, "address", c.TUN.Address, "auto_route", c.TUN.AutoRoute)
	}
	client, err := tunnel.New(ctx, c, token, locationID, dial, native)
	if err != nil {
		return err
	}
	defer client.Close()
	// Close the native descriptor before joining its reader in client.Close.
	if native != nil {
		defer native.Close()
	}
	slog.Info("HTTP / SOCKS5 proxy listening", "address", ln.Addr())
	slog.Info("Exit policy: select randomly on startup/failure; keep one healthy provider; reconnect it first")
	if locationID != "" {
		slog.Info("Exit location", "location_id", locationID)
	}
	errCh := make(chan error, 1)
	go func() { errCh <- proxy.Serve(ctx, ln, proxy.DialFunc(client.DialContext)) }()
	var tunErrors <-chan error
	if native != nil {
		tunErrors = native.Errors
	}
	select {
	case err := <-errCh:
		return err
	case err := <-tunErrors:
		cancel()
		return fmt.Errorf("TUN stopped: %w", err)
	case <-ctx.Done():
		return nil
	}
}
