package credentials

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/gotd/td/session"

	"github.com/Jaaayden/telegram-uploader/internal/fsutil"
)

// SessionStorage implements gotd session.Storage without ever writing the
// MTProto session in plaintext. The AES key lives in the OS credential store.
type SessionStorage struct {
	Secrets *Store
	Path    string
	mu      sync.RWMutex
}

func NewSessionStorage(secrets *Store, path string) *SessionStorage {
	return &SessionStorage{Secrets: secrets, Path: path}
}

func (s *SessionStorage) LoadSession(ctx context.Context) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s == nil || s.Secrets == nil || s.Path == "" {
		return nil, errors.New("加密会话存储尚未配置")
	}
	// A full lock: an unreadable session is renamed away below.
	s.mu.Lock()
	defer s.mu.Unlock()
	ciphertext, err := os.ReadFile(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, session.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("读取加密会话失败：%w", err)
	}
	key, err := s.Secrets.GetSessionKey()
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			// The encryption key is gone (new machine, cleared keychain), so the
			// stored session can never be read again. A Bot session is cheap to
			// recreate from the token, so start fresh instead of blocking Run.
			return nil, s.discardUnreadable()
		}
		return nil, fmt.Errorf("读取会话密钥失败：%w", err)
	}
	plaintext, err := decryptBytes(key, ciphertext)
	if err != nil {
		// Corrupted or tampered file, or a different key. Same recovery.
		return nil, s.discardUnreadable()
	}
	return plaintext, nil
}

// discardUnreadable moves an unusable session file aside (keeping one copy for
// diagnosis) and reports session.ErrNotFound so gotd performs a fresh Bot
// login. The caller must hold s.mu.
func (s *SessionStorage) discardUnreadable() error {
	if err := os.Rename(s.Path, s.Path+".invalid"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("清理无法读取的 Telegram 会话失败：%w", err)
	}
	return session.ErrNotFound
}

func (s *SessionStorage) StoreSession(ctx context.Context, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s == nil || s.Secrets == nil || s.Path == "" {
		return errors.New("加密会话存储尚未配置")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key, err := s.Secrets.GetOrCreateSessionKey()
	if err != nil {
		return fmt.Errorf("读取会话密钥失败：%w", err)
	}
	ciphertext, err := encryptBytes(key, data)
	if err != nil {
		return fmt.Errorf("加密 Telegram 会话失败：%w", err)
	}
	if err := fsutil.AtomicWriteFile(s.Path, ciphertext, 0o600); err != nil {
		return fmt.Errorf("保存加密 Telegram 会话失败：%w", err)
	}
	return nil
}
