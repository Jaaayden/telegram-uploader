// Package credentials contains the small amount of credential and session
// persistence used by the uploader.
package credentials

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/zalando/go-keyring"
)

const (
	// ServiceName is the default namespace used in the operating system
	// keyring.  A service namespace can be supplied to NewStore when tests or
	// an embedding application need isolation.
	ServiceName = "telegram-video-uploader"

	botTokenKey      = "bot_token"
	apiHashKey       = "api_hash"
	proxyPasswordKey = "proxy_password"
	sessionKeyKey    = "session_key"

	sessionKeySize = 32
)

// ErrNotFound means that the requested secret has not been stored yet.
//
// It is intentionally an application-level sentinel instead of exposing a
// keyring implementation error to callers.
var ErrNotFound = errors.New("credential not found")

// ErrUnavailable means that the operating-system keyring could not service
// the request.  Callers can use errors.Is to distinguish it from a missing
// secret and decide whether to show a setup/retry prompt.
var ErrUnavailable = errors.New("credential store unavailable")

// Store provides access to the application's secrets.  The zero value is
// usable and uses ServiceName.
type Store struct {
	service string

	// keyMu serializes the read/create/write sequence for the session key.  It
	// prevents two first launches in the same process from creating different
	// keys before either one has been saved.
	keyMu sync.Mutex
}

// NewStore creates a Store.  With no argument it uses ServiceName; the
// optional service argument is useful for tests and for applications that
// embed this package.
func NewStore(service ...string) *Store {
	name := ServiceName
	if len(service) > 0 && strings.TrimSpace(service[0]) != "" {
		name = service[0]
	}
	return &Store{service: name}
}

// NewStoreWithService is an explicit spelling of NewStore for callers that
// prefer not to use its optional argument.
func NewStoreWithService(service string) *Store {
	return NewStore(service)
}

func (s *Store) serviceName() string {
	if s == nil || strings.TrimSpace(s.service) == "" {
		return ServiceName
	}
	return s.service
}

func (s *Store) setSecret(name, value string) error {
	if err := keyring.Set(s.serviceName(), name, value); err != nil {
		return wrapKeyringError("set", name, err)
	}
	return nil
}

func (s *Store) getSecret(name string) (string, error) {
	value, err := keyring.Get(s.serviceName(), name)
	if err != nil {
		return "", wrapKeyringError("get", name, err)
	}
	return value, nil
}

func (s *Store) deleteSecret(name string) error {
	if err := keyring.Delete(s.serviceName(), name); err != nil {
		return wrapKeyringError("delete", name, err)
	}
	return nil
}

func wrapKeyringError(operation, name string, err error) error {
	if errors.Is(err, keyring.ErrNotFound) {
		return fmt.Errorf("%w: %s %q", ErrNotFound, operation, name)
	}
	return fmt.Errorf("%w: %s %q: %v", ErrUnavailable, operation, name, err)
}

// SetBotToken stores the Telegram bot token in the OS keyring.
func (s *Store) SetBotToken(token string) error {
	return s.setSecret(botTokenKey, token)
}

// GetBotToken retrieves the Telegram bot token from the OS keyring.
func (s *Store) GetBotToken() (string, error) {
	return s.getSecret(botTokenKey)
}

// DeleteBotToken removes the Telegram bot token from the OS keyring.
func (s *Store) DeleteBotToken() error {
	return s.deleteSecret(botTokenKey)
}

// SetAPIHash stores the Telegram API hash in the OS keyring.
func (s *Store) SetAPIHash(apiHash string) error {
	return s.setSecret(apiHashKey, apiHash)
}

// GetAPIHash retrieves the Telegram API hash from the OS keyring.
func (s *Store) GetAPIHash() (string, error) {
	return s.getSecret(apiHashKey)
}

// DeleteAPIHash removes the Telegram API hash from the OS keyring.
func (s *Store) DeleteAPIHash() error {
	return s.deleteSecret(apiHashKey)
}

// SetApiHash is kept as a spelling-compatible alias for callers that use the
// initialism casing from an existing configuration layer.
func (s *Store) SetApiHash(apiHash string) error {
	return s.SetAPIHash(apiHash)
}

// GetApiHash is kept as a spelling-compatible alias for SetApiHash.
func (s *Store) GetApiHash() (string, error) {
	return s.GetAPIHash()
}

// DeleteApiHash is kept as a spelling-compatible alias for SetApiHash.
func (s *Store) DeleteApiHash() error {
	return s.DeleteAPIHash()
}

// SetProxyPassword stores the optional proxy password in the OS keyring.
func (s *Store) SetProxyPassword(password string) error {
	return s.setSecret(proxyPasswordKey, password)
}

// GetProxyPassword retrieves the optional proxy password from the OS keyring.
func (s *Store) GetProxyPassword() (string, error) {
	return s.getSecret(proxyPasswordKey)
}

// DeleteProxyPassword removes the optional proxy password from the OS
// keyring.
func (s *Store) DeleteProxyPassword() error {
	return s.deleteSecret(proxyPasswordKey)
}

// DeleteAll removes every secret stored under this application's service
// namespace, including the session encryption key.
func (s *Store) DeleteAll() error {
	s.keyMu.Lock()
	defer s.keyMu.Unlock()
	if err := keyring.DeleteAll(s.serviceName()); err != nil {
		if errors.Is(err, keyring.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("%w: delete all credentials: %v", ErrUnavailable, err)
	}
	return nil
}

// GetOrCreateSessionKey returns the 32-byte key used for encrypting the
// Telegram session file.  The keyring stores only its standard base64 form;
// the raw key never needs to be written to disk.
func (s *Store) GetOrCreateSessionKey() ([]byte, error) {
	s.keyMu.Lock()
	defer s.keyMu.Unlock()

	if key, err := s.getSessionKey(); err == nil {
		return key, nil
	} else if !errors.Is(err, ErrNotFound) {
		return nil, err
	}

	key := make([]byte, sessionKeySize)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, fmt.Errorf("generate session key: %w", err)
	}
	encoded := base64.StdEncoding.EncodeToString(key)
	if err := s.setSecret(sessionKeyKey, encoded); err != nil {
		return nil, err
	}
	return key, nil
}

// GetSessionKey retrieves an existing session key without creating one.  It
// is primarily useful to operations such as decryption, where a missing key
// must not silently create a new key and turn a clear configuration error
// into an authentication failure.
func (s *Store) GetSessionKey() ([]byte, error) {
	s.keyMu.Lock()
	defer s.keyMu.Unlock()
	return s.getSessionKey()
}

func (s *Store) getSessionKey() ([]byte, error) {
	encoded, err := s.getSecret(sessionKeyKey)
	if err != nil {
		return nil, err
	}
	key, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("decode session key: %w", ErrUnavailable)
	}
	if len(key) != sessionKeySize {
		return nil, fmt.Errorf("decode session key: unexpected key length %d: %w", len(key), ErrUnavailable)
	}
	return append([]byte(nil), key...), nil
}

// EncryptSessionFile encrypts srcPath with AES-GCM and atomically replaces
// dstPath.  If dstPath is omitted, encryption is performed in place.  The
// destination is always written with mode 0600 and no plaintext fallback is
// attempted when encryption or keyring access fails.
func (s *Store) EncryptSessionFile(srcPath string, dstPath ...string) error {
	destination, err := destinationPath(srcPath, dstPath)
	if err != nil {
		return err
	}

	plaintext, err := os.ReadFile(srcPath)
	if err != nil {
		return wrapSessionFileReadError("read session file", srcPath, err)
	}
	key, err := s.GetOrCreateSessionKey()
	if err != nil {
		return fmt.Errorf("encrypt session file: %w", err)
	}
	ciphertext, err := encryptBytes(key, plaintext)
	if err != nil {
		return fmt.Errorf("encrypt session file: %w", err)
	}
	if err := atomicWrite0600(destination, ciphertext); err != nil {
		return fmt.Errorf("write encrypted session file: %w", err)
	}
	return nil
}

// DecryptSessionFile decrypts srcPath with AES-GCM and atomically replaces
// dstPath.  If dstPath is omitted, decryption is performed in place.  A
// malformed or tampered ciphertext is returned as an error; it is never
// interpreted as plaintext.
func (s *Store) DecryptSessionFile(srcPath string, dstPath ...string) error {
	destination, err := destinationPath(srcPath, dstPath)
	if err != nil {
		return err
	}

	ciphertext, err := os.ReadFile(srcPath)
	if err != nil {
		return wrapSessionFileReadError("read encrypted session file", srcPath, err)
	}
	key, err := s.GetSessionKey()
	if err != nil {
		return fmt.Errorf("decrypt session file: %w", err)
	}
	plaintext, err := decryptBytes(key, ciphertext)
	if err != nil {
		return fmt.Errorf("decrypt session file: %w", err)
	}
	if err := atomicWrite0600(destination, plaintext); err != nil {
		return fmt.Errorf("write decrypted session file: %w", err)
	}
	return nil
}

func destinationPath(source string, destination []string) (string, error) {
	if len(destination) > 1 {
		return "", errors.New("credentials: expected at most one destination path")
	}
	if len(destination) == 0 || strings.TrimSpace(destination[0]) == "" {
		return source, nil
	}
	return destination[0], nil
}

func encryptBytes(key, plaintext []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	// Store the nonce before the sealed payload.  The nonce is not secret and
	// keeping it beside the ciphertext makes the file self-contained.
	return aead.Seal(nonce, nonce, plaintext, nil), nil
}

func decryptBytes(key, ciphertext []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonceSize := aead.NonceSize()
	if len(ciphertext) < nonceSize+aead.Overhead() {
		return nil, errors.New("ciphertext is too short")
	}
	nonce, sealed := ciphertext[:nonceSize], ciphertext[nonceSize:]
	return aead.Open(nil, nonce, sealed, nil)
}

func atomicWrite0600(path string, contents []byte) error {
	dir := filepath.Dir(path)
	base := filepath.Base(path)
	tmp, err := os.CreateTemp(dir, "."+base+".tmp-")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	removeTemp := true
	defer func() {
		if removeTemp {
			_ = os.Remove(tmpName)
		}
	}()

	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(contents); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	removeTemp = false
	return syncParentDirectory(dir)
}

func syncParentDirectory(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	file, err := os.Open(dir)
	if err != nil {
		return err
	}
	syncErr := file.Sync()
	closeErr := file.Close()
	if syncErr != nil {
		message := strings.ToLower(syncErr.Error())
		if !strings.Contains(message, "invalid argument") &&
			!strings.Contains(message, "not supported") &&
			!strings.Contains(message, "function not implemented") {
			return syncErr
		}
	}
	return closeErr
}

func wrapSessionFileReadError(operation, path string, err error) error {
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%s %q: %w", operation, path, ErrNotFound)
	}
	return fmt.Errorf("%s %q: %w", operation, path, err)
}
