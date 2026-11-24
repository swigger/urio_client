package tunnel

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/swigger/urio-client/internal/auth"
	"github.com/swigger/urio-client/internal/config"
	"github.com/swigger/urio-client/internal/jump"
	"github.com/swigger/urio-client/internal/platform"
	"github.com/urnetwork/connect"
)

// Opt-in: current account, real discovery and independent proxy-only clients.
// No native TUN, routes, user processes, credentials, or config are modified.
func TestLiveRandomExitSelection(t *testing.T) {
	if os.Getenv("URIO_TEST_LIVE_RANDOM") != "1" {
		t.Skip("set URIO_TEST_LIVE_RANDOM=1 to use configured account")
	}
	path := os.Getenv("URIO_TEST_CONFIG")
	if path == "" {
		path = "../../config.yaml"
	}
	c, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	dial, err := Underlay(c.TUN.Interface)
	if err != nil {
		t.Fatal(err)
	}
	if c.Jump.Address != "" {
		key, err := config.Secret(c.Jump.PSK, c.Jump.PSKFile)
		if err != nil {
			t.Fatal(err)
		}
		jd := &jump.Dialer{Address: c.Jump.Address, PSK: key, Dial: dial}
		dial = jd.DialContext
	}
	transport := &http.Transport{Proxy: nil, DialContext: dial, TLSHandshakeTimeout: 15 * time.Second}
	defer transport.CloseIdleConnections()
	a := &platform.API{URL: c.APIURL, HTTP: &http.Client{Transport: transport, Timeout: 30 * time.Second}}
	token, err := auth.Login(ctx, c.Account.Email, c.Account.Password, false, auth.Keyring{Service: "urio-client:" + c.APIURL}, a.Login, nil)
	if err != nil {
		t.Fatal(err)
	}
	a.Token = token
	location, err := ResolveCity(ctx, a, c.Exit)
	if err != nil {
		t.Fatal(err)
	}
	spec := &connect.ProviderSpec{BestAvailable: true}
	if location != "" {
		id, err := connect.ParseId(location)
		if err != nil {
			t.Fatal(err)
		}
		spec = &connect.ProviderSpec{LocationId: &id}
	}
	d := &randomDiscovery{base: &strictDiscovery{api: a, locationID: location}, pickIndex: func(n int) (int, error) { t.Logf("eligible candidate pool: %d", n); return randomIndex(n) }}
	for i := 0; i < 3; i++ {
		r, err := d.FindProviders2(ctx, token, &connect.FindProviders2Args{Count: 1, RankMode: "quality", Specs: []*connect.ProviderSpec{spec}, Select: &connect.FindProvidersSelect{Ip: connect.ProviderIpCapabilityIpv4, Protocol: connect.ProviderProtocolCapabilityDualproto, TargetCount: 1}})
		if err != nil || len(r.Providers) != 1 {
			t.Fatalf("live discovery: %v", err)
		}
		t.Logf("independent draw %d: %s", i+1, r.Providers[0].ClientId)
	}
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer slog.SetDefault(old)
	for i := 0; i < 2; i++ {
		func() {
			runCtx, stop := context.WithTimeout(ctx, 60*time.Second)
			defer stop()
			client, err := New(runCtx, c, token, location, dial, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			tr := &http.Transport{Proxy: nil, DialContext: client.DialContext, DisableKeepAlives: true}
			defer tr.CloseIdleConnections()
			h := &http.Client{Transport: tr, Timeout: 45 * time.Second}
			var lastID connect.Id
			for n := 0; n < 2; n++ {
				req, _ := http.NewRequestWithContext(runCtx, http.MethodGet, "https://api.ipify.org", nil)
				res, err := h.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				body, err := io.ReadAll(io.LimitReader(res.Body, 256))
				res.Body.Close()
				if err != nil || res.StatusCode != 200 || net.ParseIP(strings.TrimSpace(string(body))) == nil {
					t.Fatalf("IP check failed: status %d, error %v", res.StatusCode, err)
				}
				var provider connect.Id
				for _, e := range client.multi.Monitor().ProviderEvents() {
					if e.State == connect.ProviderStateAdded {
						provider = e.EgressClientId
					}
				}
				if provider == (connect.Id{}) {
					t.Fatal("no admitted provider")
				}
				if n > 0 && provider != lastID {
					t.Fatal("healthy provider changed between requests")
				}
				lastID = provider
				t.Logf("client %d request %d: provider=%s exit=%s", i+1, n+1, provider, strings.TrimSpace(string(body)))
			}
		}()
	}
}
