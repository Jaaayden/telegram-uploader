package credentials

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/gotd/td/session"
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
	s.mu.RLock()
	defer s.mu.RUnlock()
	ciphertext, err := os.ReadFile(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, session.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("读取加密会话失败：%w", err)
	}
	key, err := s.Secrets.GetSessionKey()
	if err != nil {
		return nil, fmt.Errorf("读取会话密钥失败：%w", err)
	}
	plaintext, err := decryptBytes(key, ciphertext)
	if err != nil {
		return nil, fmt.Errorf("解密 Telegram 会话失败：%w", err)
	}
	return plaintext, nil
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
	if err := atomicWrite0600(s.Path, ciphertext); err != nil {
		return fmt.Errorf("保存加密 Telegram 会话失败：%w", err)
	}
	return nil
}
