package tunnel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/swigger/urio-client/internal/config"
	"github.com/swigger/urio-client/internal/platform"
	"github.com/urnetwork/connect"
)

// A city returned by search may be absent from the unfiltered listing. The
// selector's ID must still start a tunnel, with no widening to a region/country.
func TestSelectedCityIDNotInDefaultListing(t *testing.T) {
	const london = "018c8210-ce07-0f65-f306-5338d75314d6"
	discoveryCalls := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/network/find-provider-locations":
			w.Write([]byte(`{"locations":[{"location_id":"` + london + `","location_type":"city","name":"London","country":"United Kingdom","provider_count":249}]}`))
		case "/network/provider-locations":
			t.Error("startup must not validate a selected ID using the default listing")
			w.Write([]byte(`{"locations":[]}`))
		case "/network/find-providers2":
			discoveryCalls++
			var args connect.FindProviders2Args
			if err := json.NewDecoder(r.Body).Decode(&args); err != nil {
				t.Error(err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer test-token" || !args.Strict || args.ForceMinimum || args.Count != 1 || args.RankMode != "quality" || len(args.Specs) != 1 || args.Specs[0].LocationId == nil || args.Specs[0].LocationId.String() != london {
				t.Errorf("incorrect ID validation request: %+v", args)
			}
			if args.Select == nil || args.Select.Ip != connect.ProviderIpCapabilityIpv4 || args.Select.Protocol != connect.ProviderProtocolCapabilityDualproto || args.Select.TargetCount != 1 {
				t.Errorf("validation must match the running exit policy: %+v", args.Select)
			}
			w.Write([]byte(`{"providers":[{"client_id":"00000000-0000-0000-0000-000000000001","location":{"city_location_id":"` + london + `"}}]}`))
		default:
			t.Errorf("unexpected API path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer s.Close()
	a := &platform.API{URL: s.URL, HTTP: s.Client(), Token: "test-token"}
	cities, err := a.Cities(t.Context(), "london")
	if err != nil || len(cities) != 1 {
		t.Fatalf("city search: %v, %v", cities, err)
	}
	// An explicit ID takes precedence even if old name/country fields remain.
	id, err := ResolveCity(t.Context(), a, config.Exit{LocationID: strings.ToUpper(cities[0].ID), City: "old city", Country: "old country"})
	if err != nil || id != london || discoveryCalls != 1 {
		t.Fatalf("selected city: %q, %v; discovery calls: %d", id, err, discoveryCalls)
	}
}

func TestSelectedCityIDRejectsUnavailableAndFallbackProviders(t *testing.T) {
	for _, body := range []string{
		`{"providers":[]}`,
		`{"providers":[{"client_id":"00000000-0000-0000-0000-000000000001","match":"country"}]}`,
		`{"providers":[{"client_id":"00000000-0000-0000-0000-000000000001","location":{"city_location_id":"00000000-0000-0000-0000-000000000099"}}]}`,
	} {
		t.Run(body, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(body)) }))
			defer s.Close()
			a := &platform.API{URL: s.URL, HTTP: s.Client()}
			if _, err := ResolveCity(t.Context(), a, config.Exit{LocationID: "018c8210-ce07-0f65-f306-5338d75314d6"}); err == nil || !strings.Contains(err.Error(), "no online providers for exit.location_id") {
				t.Fatalf("unavailable or out-of-city provider accepted: %v", err)
			}
		})
	}
}

func TestSelectedCityIDDistinguishesAPIFailure(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	defer s.Close()
	a := &platform.API{URL: s.URL, HTTP: s.Client()}
	if _, err := ResolveCity(t.Context(), a, config.Exit{LocationID: "018c8210-ce07-0f65-f306-5338d75314d6"}); err == nil || !strings.Contains(err.Error(), "HTTP 503") || strings.Contains(err.Error(), "no online providers") {
		t.Fatalf("API failure misreported as no providers: %v", err)
	}
}

func TestStrictDiscoveryNeverFallsBack(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var a connect.FindProviders2Args
		if err := json.NewDecoder(r.Body).Decode(&a); err != nil {
			t.Fatal(err)
		}
		if !a.Strict || a.ForceMinimum {
			t.Error("fallback allowed")
		}
		w.Write([]byte(`{"providers":[]}`))
	}))
	defer s.Close()
	d := strictDiscovery{api: &platform.API{URL: s.URL, HTTP: s.Client()}}
	_, err := d.FindProviders2(t.Context(), "token", &connect.FindProviders2Args{ForceMinimum: true})
	if err != nil {
		t.Fatal(err)
	}
}
func TestCityAmbiguityAndCountry(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"locations":[{"location_type":"city","provider_count":2,"name":"Springfield","country":"USA","country_code":"us","location_id":"00000000-0000-0000-0000-000000000001"},{"location_type":"city","provider_count":1,"name":"Springfield","country":"Canada","country_code":"ca","location_id":"00000000-0000-0000-0000-000000000002"}]}`))
	}))
	defer s.Close()
	a := &platform.API{URL: s.URL, HTTP: s.Client()}
	if _, err := ResolveCity(t.Context(), a, config.Exit{City: "Springfield"}); err == nil {
		t.Fatal("ambiguous city accepted")
	}
	id, err := ResolveCity(t.Context(), a, config.Exit{City: "Springfield", Country: "CA"})
	if err != nil || id != "00000000-0000-0000-0000-000000000002" {
		t.Fatal(id, err)
	}
}

func TestDiscoveryDropsFallbackLocations(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"providers":[{"client_id":"00000000-0000-0000-0000-000000000001","match":"country"},{"client_id":"00000000-0000-0000-0000-000000000002","location":{"city_location_id":"00000000-0000-0000-0000-000000000099"}},{"client_id":"00000000-0000-0000-0000-000000000003","location":{"city_location_id":"00000000-0000-0000-0000-000000000010"}}]}`))
	}))
	defer s.Close()
	d := strictDiscovery{api: &platform.API{URL: s.URL, HTTP: s.Client()}, locationID: "00000000-0000-0000-0000-000000000010"}
	r, err := d.FindProviders2(t.Context(), "token", &connect.FindProviders2Args{})
	if err != nil || len(r.Providers) != 1 || r.Providers[0].ClientId.String() != "00000000-0000-0000-0000-000000000003" {
		t.Fatal(r, err)
	}
}
