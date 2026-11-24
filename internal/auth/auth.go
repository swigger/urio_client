package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/zalando/go-keyring"
)

var ErrPassword = errors.New("incorrect account or password")

type Store interface {
	Get(string) (string, error)
	Set(string, string) error
	Delete(string) error
}
type Keyring struct{ Service string }

func (k Keyring) Get(user string) (string, error) { return keyring.Get(k.Service, user) }
func (k Keyring) Set(user, password string) error { return keyring.Set(k.Service, user, password) }
func (k Keyring) Delete(user string) error {
	err := keyring.Delete(k.Service, user)
	if errors.Is(err, keyring.ErrNotFound) {
		return nil
	}
	return err
}

type LoginFunc func(context.Context, string, string) (string, error)
type PromptFunc func() (string, error)

func Login(ctx context.Context, email, password string, interactive bool, store Store, login LoginFunc, prompt PromptFunc) (string, error) {
	fromPrompt := false
	if password == "" {
		p, err := store.Get(email)
		if err == nil {
			password = p
		} else if !errors.Is(err, keyring.ErrNotFound) && !interactive {
			return "", fmt.Errorf("read OS credential store: %w", err)
		}
	}
	for attempt := 0; attempt < 3; attempt++ {
		if password == "" {
			if !interactive {
				return "", fmt.Errorf("no password available in config or OS credential store (--non-interactive)")
			}
			var err error
			password, err = prompt()
			if err != nil {
				return "", err
			}
			fromPrompt = true
			if password == "" {
				return "", fmt.Errorf("password cannot be empty")
			}
		}
		token, err := login(ctx, email, password)
		if err == nil {
			if fromPrompt {
				if err = store.Set(email, password); err != nil {
					return "", fmt.Errorf("authenticated, but cannot save password in OS credential store: %w", err)
				}
			}
			return token, nil
		}
		if !errors.Is(err, ErrPassword) || !interactive {
			return "", err
		}
		if err = store.Delete(email); err != nil {
			return "", fmt.Errorf("cannot clear rejected saved password: %w", err)
		}
		password = ""
	}
	return "", ErrPassword
}

// Only known, explicit credential errors trigger a new password prompt.
func IsPasswordMessage(s string) bool {
	s = strings.TrimRight(strings.ToLower(strings.TrimSpace(s)), ".!")
	for _, v := range []string{"invalid password", "incorrect password", "invalid credentials", "invalid user or password", "invalid login", "incorrect email or password", "invalid username or password"} {
		if s == v {
			return true
		}
	}
	return false
}
