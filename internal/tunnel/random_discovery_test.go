package tunnel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/swigger/urio-client/internal/platform"
	"github.com/urnetwork/connect"
)

type discoveryFunc func(context.Context, string, *connect.FindProviders2Args) (*connect.FindProviders2Result, error)

func (f discoveryFunc) FindProviders2(c context.Context, t string, a *connect.FindProviders2Args) (*connect.FindProviders2Result, error) {
	return f(c, t, a)
}
func candidate(n int) *connect.FindProvidersProvider {
	id, err := connect.ParseId(fmt.Sprintf("00000000-0000-0000-0000-%012d", n))
	if err != nil {
		panic(err)
	}
	return &connect.FindProvidersProvider{ClientId: id, Tier: n}
}

func TestRandomDiscoveryCanChooseEveryCandidateDespiteRank(t *testing.T) {
	pool := []*connect.FindProvidersProvider{candidate(1), candidate(2), candidate(3), candidate(4)}
	selector := &connect.FindProvidersSelect{Ip: connect.ProviderIpCapabilityIpv4, Protocol: connect.ProviderProtocolCapabilityDualproto, TargetCount: 1}
	result := &connect.FindProviders2Result{Providers: pool, Select: selector}
	args := &connect.FindProviders2Args{Count: 1, RankMode: "quality", Select: selector, Specs: []*connect.ProviderSpec{{BestAvailable: true}}}
	before, _ := json.Marshal(args)
	d := randomDiscovery{base: discoveryFunc(func(ctx context.Context, token string, request *connect.FindProviders2Args) (*connect.FindProviders2Result, error) {
		if request.Count != randomExitPoolSize || request.RankMode != "quality" || request.Select != selector || token != "token" {
			t.Fatalf("bad pool request: %+v", request)
		}
		return result, nil
	})}
	for chosen := range pool {
		d.pickIndex = func(n int) (int, error) {
			if n != len(pool) {
				t.Fatal(n)
			}
			return chosen, nil
		}
		r, err := d.FindProviders2(t.Context(), "token", args)
		if err != nil || len(r.Providers) != 1 || r.Providers[0] != pool[chosen] || r.Select != selector {
			t.Fatalf("rank overrode draw: %+v %v", r, err)
		}
	}
	after, _ := json.Marshal(args)
	if string(before) != string(after) || !reflect.DeepEqual(result.Providers, pool) {
		t.Fatal("modified input or upstream result")
	}
}

func TestRandomDiscoveryPreservesNamedReconnect(t *testing.T) {
	id := candidate(1).ClientId
	args := &connect.FindProviders2Args{Count: 1, RankMode: "quality", Specs: []*connect.ProviderSpec{{ClientId: &id}}}
	for _, providers := range [][]*connect.FindProvidersProvider{{candidate(1)}, nil} {
		result := &connect.FindProviders2Result{Providers: providers}
		d := randomDiscovery{base: discoveryFunc(func(_ context.Context, _ string, a *connect.FindProviders2Args) (*connect.FindProviders2Result, error) {
			if a != args || a.Count != 1 {
				t.Fatal("reconnect was broadened")
			}
			return result, nil
		}), pickIndex: func(int) (int, error) { t.Fatal("reconnect was randomized"); return 0, nil }}
		r, err := d.FindProviders2(t.Context(), "token", args)
		if err != nil || r != result {
			t.Fatal(r, err)
		}
	}
}

func TestRandomDiscoveryFiltersBeforeDrawingAndRespectsFailureExclusions(t *testing.T) {
	const city = "00000000-0000-0000-0000-000000000099"
	cityID, _ := connect.ParseId(city)
	p1, p2, p3, p4, p5, p6 := candidate(1), candidate(2), candidate(3), candidate(4), candidate(5), candidate(6)
	p1.Match = "country"
	p2.Location = &connect.ProviderLocation{CityLocationId: &p1.ClientId}
	p6.IntermediaryIds = []connect.Id{p3.ClientId}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req connect.FindProviders2Args
		json.NewDecoder(r.Body).Decode(&req)
		if !req.Strict || req.ForceMinimum || req.Count != randomExitPoolSize || len(req.ExcludeClientIds) != 1 || len(req.ExcludeDestinations) != 1 {
			t.Errorf("filters lost: %+v", req)
		}
		json.NewEncoder(w).Encode(&connect.FindProviders2Result{Providers: []*connect.FindProvidersProvider{nil, p1, p2, p3, p4, p5, p5, p6}})
	}))
	defer s.Close()
	d := randomDiscovery{base: &strictDiscovery{api: &platform.API{URL: s.URL, HTTP: s.Client()}, locationID: city}, pickIndex: func(n int) (int, error) {
		if n != 1 {
			t.Fatalf("wrong candidates after filtering: %d", n)
		}
		return 0, nil
	}}
	r, err := d.FindProviders2(t.Context(), "token", &connect.FindProviders2Args{Specs: []*connect.ProviderSpec{{LocationId: &cityID}}, ForceMinimum: true, ExcludeClientIds: []connect.Id{p3.ClientId}, ExcludeDestinations: [][]connect.Id{{p4.ClientId}}})
	if err != nil || len(r.Providers) != 1 || r.Providers[0].ClientId != p5.ClientId {
		t.Fatal(r, err)
	}
}

func TestRandomDiscoveryPrefersDualProtocolAndPropagatesErrors(t *testing.T) {
	a, b := candidate(1), candidate(2)
	a.Capabilities = &connect.ProviderCapabilities{Protocol: connect.ProviderProtocolCapabilityTcp}
	b.Capabilities = &connect.ProviderCapabilities{Protocol: connect.ProviderProtocolCapabilityDualproto}
	selector := &connect.FindProvidersSelect{Protocol: connect.ProviderProtocolCapabilityDualproto}
	result := &connect.FindProviders2Result{Providers: []*connect.FindProvidersProvider{a, b}, Select: selector}
	d := randomDiscovery{base: discoveryFunc(func(context.Context, string, *connect.FindProviders2Args) (*connect.FindProviders2Result, error) {
		return result, nil
	}), pickIndex: func(n int) (int, error) {
		if n != 1 {
			t.Fatal(n)
		}
		return 0, nil
	}}
	args := &connect.FindProviders2Args{Select: selector}
	r, err := d.FindProviders2(t.Context(), "", args)
	if err != nil || r.Providers[0] != b {
		t.Fatal(r, err)
	}
	result.Providers = []*connect.FindProvidersProvider{a}
	r, err = d.FindProviders2(t.Context(), "", args)
	if err != nil || r.Providers[0] != a {
		t.Fatal("lost TCP overflow recovery", err)
	}
	failure := errors.New("entropy unavailable")
	d.pickIndex = func(int) (int, error) { return 0, failure }
	if _, err = d.FindProviders2(t.Context(), "", args); !errors.Is(err, failure) {
		t.Fatal("must not silently choose first provider", err)
	}
	result.Providers = nil
	r, err = d.FindProviders2(t.Context(), "", args)
	if err != nil || len(r.Providers) != 0 {
		t.Fatal("empty pool must remain empty", err)
	}
	d.base = discoveryFunc(func(context.Context, string, *connect.FindProviders2Args) (*connect.FindProviders2Result, error) {
		return nil, failure
	})
	if _, err = d.FindProviders2(t.Context(), "", args); !errors.Is(err, failure) {
		t.Fatal("API error lost", err)
	}
}

func TestRandomIndexUsesFreshDraws(t *testing.T) {
	// A single shared seed/constant choice would repeatedly pick the same
	// value. The chance that 64 independent 16-way draws fail this is tiny.
	seen := map[int]bool{}
	for i := 0; i < 64; i++ {
		n, err := randomIndex(16)
		if err != nil || n < 0 || n >= 16 {
			t.Fatal(n, err)
		}
		seen[n] = true
	}
	if len(seen) < 2 {
		t.Fatal("random source repeated one candidate for every draw")
	}
}
