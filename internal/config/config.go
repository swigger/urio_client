package config

import (
	"fmt"
	"io"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/swigger/urio-client/internal/logging"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Account    Account    `yaml:"account"`
	APIURL     string     `yaml:"api_url"`
	ConnectURL string     `yaml:"connect_url"`
	Listen     string     `yaml:"listen"`
	LogLevel   string     `yaml:"log_level"`
	Exit       Exit       `yaml:"exit"`
	TUN        TUN        `yaml:"tun"`
	Jump       Jump       `yaml:"jump"`
	JumpServer JumpServer `yaml:"jump_server"`
}
type Account struct {
	Email    string `yaml:"email"`
	Password string `yaml:"password,omitempty"`
}
type Exit struct {
	City       string `yaml:"city,omitempty"`
	Country    string `yaml:"country,omitempty"`
	LocationID string `yaml:"location_id,omitempty"`
}
type TUN struct {
	Enabled     bool   `yaml:"enabled"`
	Name        string `yaml:"name"`
	Address     string `yaml:"address"`
	MTU         int    `yaml:"mtu"`
	AutoRoute   bool   `yaml:"auto_route"`
	Interface   string `yaml:"interface"`
	DNSEnabled  bool   `yaml:"dns_enabled"`
	DNSAddress  string `yaml:"dns_address"`
	DNSUpstream string `yaml:"dns_upstream"`
}
type Jump struct {
	Address string `yaml:"address"`
	PSK     string `yaml:"psk,omitempty"`
	PSKFile string `yaml:"psk_file,omitempty"`
}
type JumpServer struct {
	Listen         string `yaml:"listen"`
	PSK            string `yaml:"psk,omitempty"`
	PSKFile        string `yaml:"psk_file,omitempty"`
	AllowPrivate   bool   `yaml:"allow_private"`
	MaxConnections int    `yaml:"max_connections"`
}

func Defaults() Config {
	name := "urio"
	if runtime.GOOS == "darwin" {
		name = "utun"
	}
	return Config{APIURL: "https://api.bringyour.com", ConnectURL: "wss://connect.bringyour.com", Listen: "127.0.0.1:1080", LogLevel: "info", TUN: TUN{Name: name, Address: "172.31.255.1/30", MTU: 1280, DNSEnabled: true, DNSUpstream: "https://1.1.1.1/dns-query"}, JumpServer: JumpServer{Listen: "0.0.0.0:7443", MaxConnections: 256}}
}
func Load(path string) (Config, error) {
	c := Defaults()
	f, err := os.Open(path)
	if err != nil {
		return c, err
	}
	defer f.Close()
	d := yaml.NewDecoder(io.LimitReader(f, 1<<20))
	d.KnownFields(true)
	if err = d.Decode(&c); err != nil {
		return c, fmt.Errorf("config: %w", err)
	}
	var extra any
	if err = d.Decode(&extra); err != io.EOF {
		return c, fmt.Errorf("config must contain one YAML document")
	}
	for _, p := range []*string{&c.Jump.PSKFile, &c.JumpServer.PSKFile} {
		if *p != "" && !filepath.IsAbs(*p) {
			*p = filepath.Join(filepath.Dir(path), *p)
		}
	}
	return c, nil
}
func (c Config) Validate(mode string) error {
	if _, err := logging.Level(c.LogLevel); err != nil {
		return err
	}
	if mode == "jump-server" {
		if _, _, err := net.SplitHostPort(c.JumpServer.Listen); err != nil {
			return fmt.Errorf("jump_server.listen: %w", err)
		}
		if c.JumpServer.MaxConnections < 1 {
			return fmt.Errorf("jump_server.max_connections must be positive")
		}
		_, err := Secret(c.JumpServer.PSK, c.JumpServer.PSKFile)
		return err
	}
	if strings.TrimSpace(c.Account.Email) == "" {
		return fmt.Errorf("account.email is required")
	}
	if mode == "clear-password" {
		return nil
	}
	for _, v := range []struct{ raw, scheme string }{{c.APIURL, "https"}, {c.ConnectURL, "wss"}} {
		u, err := url.Parse(v.raw)
		if err != nil || u.Host == "" || u.Scheme != v.scheme || u.User != nil {
			return fmt.Errorf("endpoint must use %s and have a host", v.scheme)
		}
	}
	if c.Jump.Address != "" {
		if _, _, err := net.SplitHostPort(c.Jump.Address); err != nil {
			return fmt.Errorf("jump.address: %w", err)
		}
		if _, err := Secret(c.Jump.PSK, c.Jump.PSKFile); err != nil {
			return err
		}
	}
	if mode != "run" {
		return nil
	}
	if _, _, err := net.SplitHostPort(c.Listen); err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	if c.TUN.Enabled {
		p, err := netip.ParsePrefix(c.TUN.Address)
		if err != nil || !p.Addr().Is4() || p.Bits() > 30 {
			return fmt.Errorf("tun.address must be an IPv4 prefix with at least 4 addresses")
		}
		if c.TUN.MTU < 1280 || c.TUN.MTU > 9000 {
			return fmt.Errorf("tun.mtu must be 1280..9000")
		}
		if c.TUN.AutoRoute && c.TUN.Interface == "" {
			return fmt.Errorf("tun.auto_route requires tun.interface (physical uplink) to prevent routing loops")
		}
		if c.TUN.DNSEnabled {
			if _, err := c.TUN.DNSIP(); err != nil {
				return err
			}
			if err := validateDNSUpstream(c.TUN.DNSUpstream, p); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateDNSUpstream(value string, p netip.Prefix) error {
	var ip netip.Addr
	if strings.HasPrefix(value, "https://") {
		u, err := url.Parse(value)
		if err != nil || u.User != nil || u.Fragment != "" || u.Hostname() == "" {
			return fmt.Errorf("invalid tun.dns_upstream HTTPS URL")
		}
		ip, err = netip.ParseAddr(u.Hostname())
		if err != nil {
			return fmt.Errorf("tun.dns_upstream HTTPS host must be a literal public IPv4 address")
		}
	} else {
		u, err := netip.ParseAddrPort(value)
		if err != nil || u.Port() == 0 {
			return fmt.Errorf("tun.dns_upstream must be an HTTPS URL or public IPv4 address:port")
		}
		ip = u.Addr()
	}
	if !ip.Is4() || !ip.IsGlobalUnicast() || ip.IsPrivate() || p.Contains(ip) {
		return fmt.Errorf("tun.dns_upstream must use a public IPv4 address")
	}
	return nil
}

// The virtual DNS peer must not be assigned to a host interface: packets must
// reach the TUN reader, rather than the host's DNS / Windows ICS service.
func (c TUN) DNSIP() (netip.Addr, error) {
	p, err := netip.ParsePrefix(c.Address)
	if err != nil || !p.Addr().Is4() || p.Bits() > 30 {
		return netip.Addr{}, fmt.Errorf("invalid tun.address")
	}
	ip := p.Masked().Addr().Next()
	if ip == p.Addr() {
		ip = ip.Next()
	}
	if c.DNSAddress != "" {
		ip, err = netip.ParseAddr(c.DNSAddress)
		if err != nil {
			return netip.Addr{}, fmt.Errorf("tun.dns_address: %w", err)
		}
	}
	if !ip.Is4() || !p.Contains(ip) || ip == p.Addr() || ip == p.Masked().Addr() || !p.Contains(ip.Next()) {
		return netip.Addr{}, fmt.Errorf("tun.dns_address must be an unused host address in tun.address's subnet")
	}
	return ip, nil
}
func Secret(value, path string) ([]byte, error) {
	if value != "" && path != "" {
		return nil, fmt.Errorf("set only psk or psk_file")
	}
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		value = strings.TrimSpace(string(b))
	}
	if len(value) < 32 {
		return nil, fmt.Errorf("jump PSK must contain at least 32 bytes; use a randomly generated secret")
	}
	return []byte(value), nil
}
