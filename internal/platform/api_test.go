package platform

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/swigger/urio-client/internal/auth"
)

func TestLoginFailures(t *testing.T) {
	for _, tc := range []struct {
		body     string
		status   int
		password bool
	}{{`{"error":{"message":"Invalid password"}}`, 200, true}, {`{"error":{"message":"Invalid user or password."}}`, 200, true}, {`{"verification_required":{"user_auth":"u"}}`, 200, false}, {`{"error":{"message":"Account locked"}}`, 200, false}, {`{}`, 503, false}} {
		t.Run(tc.body, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.status); w.Write([]byte(tc.body)) }))
			defer s.Close()
			a := API{URL: s.URL, HTTP: s.Client()}
			_, err := a.Login(t.Context(), "u", "p")
			if err == nil || errors.Is(err, auth.ErrPassword) != tc.password {
				t.Fatal(err)
			}
		})
	}
}
func TestCityOnlineFilter(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer token" {
			t.Error("missing auth")
		}
		w.Write([]byte(`{"locations":[{"location_type":"city","provider_count":1,"name":"A"},{"location_type":"country","provider_count":2},{"location_type":"city","provider_count":0}]}`))
	}))
	defer s.Close()
	a := API{URL: s.URL, HTTP: s.Client(), Token: "token"}
	cities, err := a.Cities(t.Context(), "")
	if err != nil || len(cities) != 1 || cities[0].Name != "A" {
		t.Fatal(cities, err)
	}
}
