package credentials

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/gotd/td/session"
	"github.com/zalando/go-keyring"
)

func TestSessionStorageEncryptedRoundTrip(t *testing.T) {
	keyring.MockInit()
	path := filepath.Join(t.TempDir(), "session.enc")
	storage := NewSessionStorage(NewStore("session-storage-test"), path)

	if _, err := storage.LoadSession(context.Background()); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("missing session error = %v", err)
	}
	want := []byte(`{"auth_key":"plain-session-marker"}`)
	if err := storage.StoreSession(context.Background(), want); err != nil {
		t.Fatal(err)
	}
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(onDisk, []byte("plain-session-marker")) {
		t.Fatal("encrypted session contains plaintext")
	}
	got, err := storage.LoadSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("session = %q, want %q", got, want)
	}
}
