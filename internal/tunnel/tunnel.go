package tunnel

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"runtime"
	"sync"
	"time"

	"github.com/swigger/urio-client/internal/config"
	"github.com/swigger/urio-client/internal/platform"
	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
)

type Client struct {
	Stack              *connect.Tun
	multi              *connect.RemoteUserNatMultiClient
	generator          *connect.ApiMultiClientGenerator
	strategy           *connect.ClientStrategy
	cancel             context.CancelFunc
	wg                 sync.WaitGroup
	closeOnce          sync.Once
	native             *Native
	discoveryTransport *http.Transport
	dns                *tunnelDNS
	stopMonitor        func()
}

func New(ctx context.Context, c config.Config, token, locationID string, dial func(context.Context, string, string) (net.Conn, error), native *Native) (*Client, error) {
	ctx, cancel := context.WithCancel(ctx)
	log := connectLogger{secrets: []string{token, c.Account.Password, c.Jump.PSK}}
	strategySettings := connect.DefaultClientStrategySettings()
	strategySettings.Log = log
	strategySettings.EnableResilient = false
	strategySettings.ConnectSettings.DialContextSettings = &connect.DialContextSettings{DialContext: dial, PacketConnFactory: func(context.Context) (net.PacketConn, error) { return nil, fmt.Errorf("UDP underlay disabled; use H1") }}
	strategy := connect.NewClientStrategy(ctx, strategySettings)
	settings := connect.DefaultTunSettings()
	settings.Log = log
	stack, err := connect.CreateTun(ctx, settings)
	if err != nil {
		strategy.Close()
		cancel()
		return nil, err
	}
	spec := &connect.ProviderSpec{BestAvailable: true}
	if locationID != "" {
		id, err := connect.ParseId(locationID)
		if err != nil {
			stack.Close()
			strategy.Close()
			cancel()
			return nil, err
		}
		spec = &connect.ProviderSpec{LocationId: &id}
	}
	gs := connect.DefaultApiMultiClientGeneratorSettings()
	gs.PlatformTransportMode = connect.TransportModeH1
	discoveryTransport := &http.Transport{Proxy: nil, DialContext: dial, TLSHandshakeTimeout: 15 * time.Second}
	gs.ProviderDiscovery = &randomDiscovery{base: &strictDiscovery{api: &platform.API{URL: c.APIURL, HTTP: &http.Client{Transport: discoveryTransport, Timeout: 30 * time.Second}}, locationID: locationID}}
	g := connect.NewApiMultiClientGenerator(ctx, []*connect.ProviderSpec{spec}, strategy, nil, c.APIURL, token, c.ConnectURL, "urio-client", runtime.GOOS, "0.1.0", nil, func() *connect.ClientSettings { s := connect.DefaultClientSettings(); s.Log = log; return s }, gs)
	ms := singleExitSettings()
	ms.Log = log
	no := false
	ms.OverrideAllowDirect = &no
	ms.SecurityPolicyGenerator = connect.DisableSecurityPolicyWithStats
	// No local NAT escape: every accepted proxy and TUN flow uses a provider.
	ms.LocalDialContextSettings = &connect.DialContextSettings{DialContext: func(context.Context, string, string) (net.Conn, error) {
		return nil, fmt.Errorf("local egress is disabled")
	}}
	local := map[netip.Addr]bool{}
	for _, a := range stack.LocalAddresses() {
		local[a] = true
	}
	packets := &packetLog{}
	receive := func(_ connect.TransferPath, _ protocol.ProvideMode, _ *connect.IpPath, p []byte) {
		var dst netip.Addr
		if len(p) >= 20 && p[0]>>4 == 4 {
			dst = netip.AddrFrom4([4]byte(p[16:20]))
		} else if len(p) >= 40 && p[0]>>4 == 6 {
			dst = netip.AddrFrom16([16]byte(p[24:40]))
		} else {
			return
		}
		if local[dst] {
			packets.packet(p, true, false)
			stack.Write(p)
		} else if native != nil {
			packets.packet(p, true, true)
			native.WritePacket(p)
		}
	}
	multi := connect.NewRemoteUserNatMultiClient(ctx, g, receive, protocol.ProvideMode_Public, ms)
	client := &Client{Stack: stack, multi: multi, generator: g, strategy: strategy, cancel: cancel, native: native, discoveryTransport: discoveryTransport}
	exits := &exitLog{}
	client.stopMonitor = multi.Monitor().AddMonitorEventCallback(exits.update)
	if native != nil && c.TUN.DNSEnabled {
		client.dns, err = newTunnelDNS(ctx, stack, c.TUN, native.WritePacket)
		if err != nil {
			client.Close()
			return nil, fmt.Errorf("TUN DNS: %w", err)
		}
	}
	client.wg.Add(1)
	go func() { defer client.wg.Done(); packets.run(ctx) }()
	source := connect.SourceId(connect.NewId())
	client.wg.Add(1)
	go func() {
		defer client.wg.Done()
		for {
			p, err := stack.Read()
			if err != nil {
				return
			}
			if client.dns != nil && client.dns.output(p) {
				connect.MessagePoolReturn(p)
				continue
			}
			packets.packet(p, false, false)
			if !multi.SendPacket(source, protocol.ProvideMode_Network, p, -1) {
				packets.rejected(p, ctx)
				connect.MessagePoolReturn(p)
			}
		}
	}()
	if native != nil {
		client.wg.Add(1)
		go func() {
			defer client.wg.Done()
			b := make([]byte, 65535)
			for {
				n, err := native.ReadPacket(b)
				if err != nil {
					if ctx.Err() == nil {
						slog.Error("[TUN] read failed", "error", err)
						native.Report(err)
					}
					return
				}
				if client.dns != nil && client.dns.input(b[:n]) {
					continue
				}
				packets.packet(b[:n], false, true)
				p := connect.MessagePoolGet(n)
				copy(p, b[:n])
				if !multi.SendPacket(source, protocol.ProvideMode_Network, p, -1) {
					packets.rejected(p, ctx)
					connect.MessagePoolReturn(p)
				}
			}
		}()
	}
	return client, nil
}
func (c *Client) Close() {
	c.closeOnce.Do(func() {
		c.cancel()
		if c.stopMonitor != nil {
			c.stopMonitor()
		}
		if c.dns != nil {
			c.dns.Close()
		}
		if c.native != nil {
			c.native.Close()
		}
		c.Stack.Close()
		c.multi.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		c.generator.CloseAndWait(ctx)
		c.strategy.Close()
		c.discoveryTransport.CloseIdleConnections()
		c.wg.Wait()
	})
}

func (l *packetLog) rejected(p []byte, ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	path, err := connect.ParseIpPath(p)
	if err != nil {
		return
	}
	k := pathKey(path, false)
	if path.DestinationIp.IsMulticast() || path.DestinationIp.Equal(net.IPv4bcast) {
		slog.Debug("[TUN] local broadcast/multicast dropped", "destination", k.destination)
		return
	}
	// A provider without IPv6 can reject many parallel resolver probes and TCP
	// retries. Report once per destination per 30s, including the suppressed count.
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.drops == nil {
		l.drops = make(map[string]dropLog)
	}
	key := k.network + " " + k.destination
	now := time.Now()
	state := l.drops[key]
	state.count++
	if now.Sub(state.last) >= 30*time.Second {
		slog.Warn("[CONN] tunnel rejected packets", "network", k.network, "source", k.source, "destination", k.destination, "count", state.count)
		state.last = now
		state.count = 0
	}
	if len(l.drops) >= 1024 {
		for key, value := range l.drops {
			if now.Sub(value.last) > time.Minute {
				delete(l.drops, key)
			}
		}
	}
	if len(l.drops) < 1024 {
		l.drops[key] = state
	}
}

type strictDiscovery struct {
	api        *platform.API
	locationID string
}

func (d *strictDiscovery) FindProviders2(ctx context.Context, token string, args *connect.FindProviders2Args) (*connect.FindProviders2Result, error) {
	copyArgs := *args
	copyArgs.Strict = true
	copyArgs.ForceMinimum = false
	a := *d.api
	a.Token = token
	var result connect.FindProviders2Result
	if err := a.Call(ctx, "/network/find-providers2", &copyArgs, &result); err != nil {
		return nil, err
	}
	// Reject a server-side fallback even if an older endpoint ignores Strict.
	if d.locationID != "" {
		providers := result.Providers[:0]
		for _, p := range result.Providers {
			if p == nil {
				continue
			}
			if p.Match != "" {
				continue
			}
			if p.Location != nil && p.Location.CityLocationId != nil && p.Location.CityLocationId.String() != d.locationID {
				continue
			}
			providers = append(providers, p)
		}
		result.Providers = providers
	}
	return &result, nil
}

func ResolveCity(ctx context.Context, a *platform.API, e config.Exit) (string, error) {
	if e.LocationID != "" {
		id, err := connect.ParseId(e.LocationID)
		if err != nil {
			return "", fmt.Errorf("exit.location_id: %w", err)
		}
		// The default location listing is not a complete city index. IDs
		// returned by city search must be checked against provider discovery,
		// which is also the authority the running tunnel uses for this ID.
		canonicalID := id.String()
		discovery := strictDiscovery{api: a, locationID: canonicalID}
		selectors := singleExitSettings().NormalWindowSelectors
		result, err := discovery.FindProviders2(ctx, a.Token, &connect.FindProviders2Args{
			Specs:    []*connect.ProviderSpec{{LocationId: &id}},
			Count:    1,
			RankMode: "quality",
			Select: &connect.FindProvidersSelect{
				Ip: selectors.Ip, Protocol: selectors.Protocol, TargetCount: 1,
			},
		})
		if err != nil {
			return "", fmt.Errorf("check exit.location_id %s: %w", canonicalID, err)
		}
		if len(result.Providers) == 0 {
			return "", fmt.Errorf("no online providers for exit.location_id %s; retry later or run cities to choose another city", canonicalID)
		}
		return canonicalID, nil
	}
	if e.City == "" {
		return "", nil
	}
	cities, err := a.Cities(ctx, e.City)
	if err != nil {
		return "", err
	}
	var matches []platform.Location
	for _, l := range cities {
		if equal(l.City, e.City) || equal(l.Name, e.City) {
			if e.Country == "" || equal(l.Country, e.Country) || equal(l.CountryCode, e.Country) {
				matches = append(matches, l)
			}
		}
	}
	if len(matches) == 0 {
		return "", fmt.Errorf("no online providers for configured city; run cities to choose an available location")
	}
	if len(matches) > 1 {
		return "", fmt.Errorf("city is ambiguous; set exit.country or select exit.location_id with cities")
	}
	return matches[0].ID, nil
}
