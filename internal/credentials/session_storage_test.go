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

func TestSessionStorageRecoversFromUnreadableSession(t *testing.T) {
	tests := []struct {
		name    string
		breakIt func(t *testing.T, store *Store, path string)
	}{
		{
			name: "corrupted file",
			breakIt: func(t *testing.T, _ *Store, path string) {
				if err := os.WriteFile(path, bytes.Repeat([]byte{0x42}, 64), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "missing key",
			breakIt: func(t *testing.T, store *Store, _ string) {
				if err := store.DeleteAll(); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			keyring.MockInit()
			path := filepath.Join(t.TempDir(), "session.enc")
			store := NewStore("session-recovery-" + tt.name)
			storage := NewSessionStorage(store, path)
			if err := storage.StoreSession(context.Background(), []byte("session")); err != nil {
				t.Fatal(err)
			}
			tt.breakIt(t, store, path)

			if _, err := storage.LoadSession(context.Background()); !errors.Is(err, session.ErrNotFound) {
				t.Fatalf("LoadSession() error = %v, want session.ErrNotFound", err)
			}
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("unreadable session still at original path: %v", err)
			}
			if _, err := os.Stat(path + ".invalid"); err != nil {
				t.Fatalf("unreadable session was not kept aside: %v", err)
			}
			// A fresh login must be able to store a new session afterwards.
			if err := storage.StoreSession(context.Background(), []byte("fresh")); err != nil {
				t.Fatal(err)
			}
			got, err := storage.LoadSession(context.Background())
			if err != nil || string(got) != "fresh" {
				t.Fatalf("after recovery got %q, err %v", got, err)
			}
		})
	}
}

func TestSessionStorageKeepsSessionWhenKeyringUnavailable(t *testing.T) {
	keyring.MockInitWithError(errors.New("keychain locked"))
	path := filepath.Join(t.TempDir(), "session.enc")
	if err := os.WriteFile(path, bytes.Repeat([]byte{1}, 64), 0o600); err != nil {
		t.Fatal(err)
	}
	storage := NewSessionStorage(NewStore("session-unavailable"), path)
	_, err := storage.LoadSession(context.Background())
	if err == nil || errors.Is(err, session.ErrNotFound) {
		t.Fatalf("LoadSession() error = %v, want a keyring error", err)
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Fatalf("session must be kept when the keyring is merely unavailable: %v", statErr)
	}
}
