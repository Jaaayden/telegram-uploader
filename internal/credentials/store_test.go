package credentials

import (
	"bytes"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/zalando/go-keyring"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	keyring.MockInit()
	return NewStore("telegram-video-uploader-test-" + t.Name())
}

func TestStoreSecrets(t *testing.T) {
	store := testStore(t)

	tests := []struct {
		name   string
		set    func(string) error
		get    func() (string, error)
		delete func() error
		value  string
	}{
		{
			name:   "bot token",
			set:    store.SetBotToken,
			get:    store.GetBotToken,
			delete: store.DeleteBotToken,
			value:  "123456:bot-token",
		},
		{
			name:   "api hash",
			set:    store.SetAPIHash,
			get:    store.GetAPIHash,
			delete: store.DeleteAPIHash,
			value:  "api-hash",
		},
		{
			name:   "proxy password",
			set:    store.SetProxyPassword,
			get:    store.GetProxyPassword,
			delete: store.DeleteProxyPassword,
			value:  "proxy-password",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tc.get(); !errors.Is(err, ErrNotFound) {
				t.Fatalf("get before set error = %v, want ErrNotFound", err)
			}
			if err := tc.set(tc.value); err != nil {
				t.Fatalf("set: %v", err)
			}
			got, err := tc.get()
			if err != nil {
				t.Fatalf("get after set: %v", err)
			}
			if got != tc.value {
				t.Fatalf("get = %q, want %q", got, tc.value)
			}
			if err := tc.delete(); err != nil {
				t.Fatalf("delete: %v", err)
			}
			if _, err := tc.get(); !errors.Is(err, ErrNotFound) {
				t.Fatalf("get after delete error = %v, want ErrNotFound", err)
			}
		})
	}
}

func TestStoreWrapsUnavailableKeyring(t *testing.T) {
	keyring.MockInitWithError(errors.New("backend unavailable"))
	store := NewStore("telegram-video-uploader-unavailable-test")

	if _, err := store.GetBotToken(); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("get with unavailable keyring error = %v, want ErrUnavailable", err)
	}
}

func TestStoreSessionKeyIsBase64AndStable(t *testing.T) {
	store := testStore(t)

	first, err := store.GetOrCreateSessionKey()
	if err != nil {
		t.Fatalf("create session key: %v", err)
	}
	if len(first) != sessionKeySize {
		t.Fatalf("session key length = %d, want %d", len(first), sessionKeySize)
	}

	second, err := store.GetOrCreateSessionKey()
	if err != nil {
		t.Fatalf("get session key: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("GetOrCreateSessionKey returned a different key")
	}

	encoded, err := keyring.Get(store.serviceName(), sessionKeyKey)
	if err != nil {
		t.Fatalf("read encoded session key from mock keyring: %v", err)
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("decode stored session key: %v", err)
	}
	if !bytes.Equal(decoded, first) {
		t.Fatal("stored session key does not match returned key")
	}
}

func TestSessionFileRoundTripAndPermissions(t *testing.T) {
	store := testStore(t)
	dir := t.TempDir()
	plainPath := filepath.Join(dir, "session.plain")
	encryptedPath := filepath.Join(dir, "session.enc")
	decryptedPath := filepath.Join(dir, "session.dec")
	plaintext := []byte("session database bytes: not safe to leave in the encrypted file")

	if err := os.WriteFile(plainPath, plaintext, 0o600); err != nil {
		t.Fatalf("write plaintext fixture: %v", err)
	}
	if err := store.EncryptSessionFile(plainPath, encryptedPath); err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	assert0600(t, encryptedPath)
	encrypted, err := os.ReadFile(encryptedPath)
	if err != nil {
		t.Fatalf("read encrypted file: %v", err)
	}
	if bytes.Contains(encrypted, plaintext) {
		t.Fatal("encrypted file still contains the plaintext")
	}

	if err := store.DecryptSessionFile(encryptedPath, decryptedPath); err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	assert0600(t, decryptedPath)
	decrypted, err := os.ReadFile(decryptedPath)
	if err != nil {
		t.Fatalf("read decrypted file: %v", err)
	}
	if !bytes.Equal(decrypted, plaintext) {
		t.Fatalf("decrypted bytes = %q, want %q", decrypted, plaintext)
	}
}

func TestDecryptSessionFileRejectsTamperingWithoutReplacingDestination(t *testing.T) {
	store := testStore(t)
	dir := t.TempDir()
	plainPath := filepath.Join(dir, "session.plain")
	encryptedPath := filepath.Join(dir, "session.enc")
	decryptedPath := filepath.Join(dir, "session.dec")
	plaintext := []byte("session payload")
	untouched := []byte("keep this destination")

	if err := os.WriteFile(plainPath, plaintext, 0o600); err != nil {
		t.Fatalf("write plaintext fixture: %v", err)
	}
	if err := store.EncryptSessionFile(plainPath, encryptedPath); err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	encrypted, err := os.ReadFile(encryptedPath)
	if err != nil {
		t.Fatalf("read encrypted file: %v", err)
	}
	encrypted[len(encrypted)-1] ^= 1
	if err := os.WriteFile(encryptedPath, encrypted, 0o600); err != nil {
		t.Fatalf("write tampered file: %v", err)
	}
	if err := os.WriteFile(decryptedPath, untouched, 0o600); err != nil {
		t.Fatalf("write destination fixture: %v", err)
	}

	if err := store.DecryptSessionFile(encryptedPath, decryptedPath); err == nil {
		t.Fatal("decrypting tampered ciphertext succeeded")
	}
	got, err := os.ReadFile(decryptedPath)
	if err != nil {
		t.Fatalf("read destination after failed decrypt: %v", err)
	}
	if !bytes.Equal(got, untouched) {
		t.Fatalf("destination changed after failed decrypt: %q", got)
	}
}

func TestSessionFileInPlaceEncryptionDoesNotLeavePlaintext(t *testing.T) {
	store := testStore(t)
	path := filepath.Join(t.TempDir(), "session.db")
	plaintext := []byte("private session contents")
	if err := os.WriteFile(path, plaintext, 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if err := store.EncryptSessionFile(path); err != nil {
		t.Fatalf("encrypt in place: %v", err)
	}
	assert0600(t, path)
	ciphertext, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read ciphertext: %v", err)
	}
	if bytes.Contains(ciphertext, plaintext) {
		t.Fatal("in-place encrypted file still contains plaintext")
	}
	if err := store.DecryptSessionFile(path); err != nil {
		t.Fatalf("decrypt in place: %v", err)
	}
	assert0600(t, path)
	decrypted, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read decrypted file: %v", err)
	}
	if !bytes.Equal(decrypted, plaintext) {
		t.Fatalf("in-place roundtrip = %q, want %q", decrypted, plaintext)
	}
}

func assert0600(t *testing.T, path string) {
	t.Helper()
	// Windows exposes ACL-backed files with synthesized Unix permission bits,
	// commonly 0666. os.FileMode therefore cannot verify Windows file privacy.
	if runtime.GOOS == "windows" {
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %q: %v", path, err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("mode of %q = %o, want 600", path, got)
	}
}
