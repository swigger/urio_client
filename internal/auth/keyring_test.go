package auth

import (
	"fmt"
	"os"
	"testing"
	"time"
)

func TestOSCredentialStore(t *testing.T) {
	if os.Getenv("URIO_TEST_KEYRING") != "1" {
		t.Skip("requires an unlocked OS credential store")
	}
	k := Keyring{Service: fmt.Sprintf("urio-client-test-%d", time.Now().UnixNano())}
	defer k.Delete("test-user")
	if err := k.Set("test-user", "temporary-test-secret"); err != nil {
		t.Fatal(err)
	}
	p, err := k.Get("test-user")
	if err != nil || p != "temporary-test-secret" {
		t.Fatal("credential roundtrip failed", err)
	}
	if err = k.Delete("test-user"); err != nil {
		t.Fatal(err)
	}
	if _, err = k.Get("test-user"); err == nil {
		t.Fatal("credential survived deletion")
	}
}
