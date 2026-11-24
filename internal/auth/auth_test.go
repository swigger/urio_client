package auth

import (
	"context"
	"errors"
	"testing"

	"github.com/zalando/go-keyring"
)

type memoryStore struct {
	password        string
	writes, deletes int
	err             error
}

func (m *memoryStore) Get(string) (string, error) {
	if m.err != nil {
		return "", m.err
	}
	if m.password == "" {
		return "", keyring.ErrNotFound
	}
	return m.password, nil
}
func (m *memoryStore) Set(_, p string) error { m.writes++; m.password = p; return m.err }
func (m *memoryStore) Delete(string) error   { m.deletes++; m.password = ""; return m.err }
func TestLoginPasswordLifecycle(t *testing.T) {
	s := &memoryStore{password: "old"}
	prompts := 0
	attempts := 0
	got, err := Login(t.Context(), "user", "", true, s, func(_ context.Context, _, p string) (string, error) {
		attempts++
		if p != "new" {
			return "", ErrPassword
		}
		return "jwt", nil
	}, func() (string, error) { prompts++; return "new", nil })
	if err != nil || got != "jwt" || s.password != "new" || prompts != 1 || attempts != 2 || s.deletes != 1 || s.writes != 1 {
		t.Fatalf("got %q, %v, store=%+v prompts=%d attempts=%d", got, err, s, prompts, attempts)
	}
}
func TestNonInteractiveNeverPrompts(t *testing.T) {
	for _, password := range []string{"", "wrong"} {
		s := &memoryStore{}
		_, err := Login(t.Context(), "u", password, false, s, func(context.Context, string, string) (string, error) { return "", ErrPassword }, func() (string, error) { t.Fatal("prompted"); return "", nil })
		if err == nil {
			t.Fatal("expected error")
		}
	}
}
func TestNetworkErrorDoesNotErasePassword(t *testing.T) {
	s := &memoryStore{password: "keep"}
	networkErr := errors.New("network unavailable")
	_, err := Login(t.Context(), "u", "", true, s, func(context.Context, string, string) (string, error) { return "", networkErr }, func() (string, error) { t.Fatal("prompted on network error"); return "", nil })
	if !errors.Is(err, networkErr) || s.deletes != 0 {
		t.Fatal(err, s)
	}
}
func TestExplicitConfigPasswordTakesPrecedence(t *testing.T) {
	s := &memoryStore{password: "stale"}
	_, err := Login(t.Context(), "u", "config", false, s, func(_ context.Context, _, p string) (string, error) {
		if p != "config" {
			t.Fatal(p)
		}
		return "token", nil
	}, nil)
	if err != nil || s.writes != 0 {
		t.Fatal(err, s)
	}
}
func TestCredentialStoreFailureIsReported(t *testing.T) {
	s := &memoryStore{err: errors.New("locked")}
	_, err := Login(t.Context(), "u", "", true, s, func(context.Context, string, string) (string, error) { return "token", nil }, func() (string, error) { return "password", nil })
	if err == nil {
		t.Fatal("silently lost password")
	}
}
