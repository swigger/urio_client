package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConfigStrictAndDefaults(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	os.WriteFile(p, []byte("account:\n  email: user@example.com\n"), 0600)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.TUN.AutoRoute || c.TUN.Enabled || c.Listen != "127.0.0.1:1080" {
		t.Fatal(c)
	}
	if err = c.Validate("run"); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(p, []byte("tun:\n  auto_routes: true\n"), 0600)
	if _, err = Load(p); err == nil {
		t.Fatal("unknown key accepted")
	}
}
func TestAutoRouteNeedsUplink(t *testing.T) {
	c := Defaults()
	c.Account.Email = "user"
	c.TUN.Enabled = true
	c.TUN.AutoRoute = true
	if err := c.Validate("run"); err == nil {
		t.Fatal("route loop risk accepted")
	}
	c.TUN.Interface = "test"
	if err := c.Validate("run"); err != nil {
		t.Fatal(err)
	}
}

func TestVirtualDNSAddressValidation(t *testing.T) {
	c := Defaults()
	c.Account.Email = "user"
	c.TUN.Enabled = true
	ip, err := c.TUN.DNSIP()
	if err != nil || ip.String() != "172.31.255.2" {
		t.Fatal(ip, err)
	}
	for _, address := range []string{"172.31.255.1", "172.31.255.0", "172.31.255.3", "192.168.1.2", "bad"} {
		c.TUN.DNSAddress = address
		if err := c.Validate("run"); err == nil {
			t.Fatalf("invalid DNS address accepted: %s", address)
		}
	}
	c.TUN.DNSAddress = "172.31.255.2"
	c.TUN.DNSUpstream = "172.31.255.2:53"
	if c.Validate("run") == nil {
		t.Fatal("DNS loop accepted")
	}
	c.TUN.DNSEnabled = false
	if err := c.Validate("run"); err != nil {
		t.Fatal(err)
	}
	c.LogLevel = "verbose"
	if c.Validate("run") == nil {
		t.Fatal("invalid log level accepted")
	}
}
func TestPSKFileRelativeToConfig(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "key"), []byte("12345678901234567890123456789012"), 0600)
	p := filepath.Join(dir, "config.yaml")
	os.WriteFile(p, []byte("jump_server:\n  psk_file: key\n"), 0600)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Validate("jump-server"); err != nil {
		t.Fatal(err)
	}
}
