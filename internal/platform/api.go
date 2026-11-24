package platform

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/swigger/urio-client/internal/auth"
)

type API struct {
	URL   string
	HTTP  *http.Client
	Token string
}

func (a *API) Call(ctx context.Context, path string, args, out any) error {
	var body io.Reader
	method := "GET"
	if args != nil {
		b, err := json.Marshal(args)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
		method = "POST"
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(a.URL, "/")+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if a.Token != "" {
		req.Header.Set("Authorization", "Bearer "+a.Token)
	}
	res, err := a.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("API request: %w", err)
	}
	defer res.Body.Close()
	b, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if err != nil {
		return err
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		if path == "/auth/login-with-password" && res.StatusCode == 401 {
			return auth.ErrPassword
		}
		return fmt.Errorf("API %s returned HTTP %d", path, res.StatusCode)
	}
	if err = json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("decode API response: %w", err)
	}
	return nil
}
func (a *API) Login(ctx context.Context, email, password string) (string, error) {
	var r struct {
		Network *struct {
			Token string `json:"by_jwt"`
		} `json:"network"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
		Verification json.RawMessage `json:"verification_required"`
	}
	err := a.Call(ctx, "/auth/login-with-password", map[string]string{"user_auth": email, "password": password}, &r)
	if err != nil {
		return "", err
	}
	if r.Error != nil {
		if auth.IsPasswordMessage(r.Error.Message) {
			return "", auth.ErrPassword
		}
		return "", fmt.Errorf("login: %s", r.Error.Message)
	}
	if r.Network == nil || r.Network.Token == "" {
		if len(r.Verification) > 0 && string(r.Verification) != "null" {
			return "", fmt.Errorf("account verification required; verify the account at ur.io")
		}
		return "", fmt.Errorf("login response did not contain a token")
	}
	return r.Network.Token, nil
}

type Location struct {
	ID          string `json:"location_id"`
	Type        string `json:"location_type"`
	Name        string `json:"name"`
	City        string `json:"city"`
	Country     string `json:"country"`
	CountryCode string `json:"country_code"`
	Region      string `json:"region"`
	Count       int    `json:"provider_count"`
}

func (a *API) Cities(ctx context.Context, query string) ([]Location, error) {
	var r struct {
		Locations []Location `json:"locations"`
	}
	var args any
	path := "/network/provider-locations"
	if query != "" {
		path = "/network/find-provider-locations"
		args = map[string]any{"query": query, "enable_max_distance_fraction": true, "max_distance_fraction": 0}
	}
	if err := a.Call(ctx, path, args, &r); err != nil {
		return nil, err
	}
	var cities []Location
	for _, l := range r.Locations {
		if l.Type == "city" && l.Count > 0 {
			cities = append(cities, l)
		}
	}
	return cities, nil
}
