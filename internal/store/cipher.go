package store

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// FieldCipher provides AES-GCM encryption for sensitive fields.
type FieldCipher struct {
	mu  sync.Mutex
	gcm cipher.AEAD
}

func newFieldCipher(dataDir string) (*FieldCipher, error) {
	if strings.TrimSpace(dataDir) == "" {
		dataDir = "./data"
	}
	keyPath := filepath.Join(dataDir, ".field-key")
	key, err := loadOrCreateKey(keyPath)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &FieldCipher{gcm: gcm}, nil
}

func loadOrCreateKey(path string) ([]byte, error) {
	if b, err := os.ReadFile(path); err == nil {
		if len(b) != 32 {
			return nil, errors.New("invalid field encryption key length")
		}
		return b, nil
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	key := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, key, 0o600); err != nil {
		return nil, err
	}
	return key, nil
}

func (c *FieldCipher) encrypt(plaintext string) string {
	if c == nil || plaintext == "" || strings.HasPrefix(plaintext, "enc:") {
		return plaintext
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	nonce := make([]byte, c.gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return plaintext
	}
	ciphertext := c.gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return "enc:" + hex.EncodeToString(ciphertext)
}

func (c *FieldCipher) decrypt(ciphertext string) (string, error) {
	if c == nil || ciphertext == "" || !strings.HasPrefix(ciphertext, "enc:") {
		return ciphertext, nil
	}
	raw, err := hex.DecodeString(strings.TrimPrefix(ciphertext, "enc:"))
	if err != nil {
		return "", err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	nonceSize := c.gcm.NonceSize()
	if len(raw) < nonceSize {
		return "", errors.New("encrypted field is too short")
	}
	plaintext, err := c.gcm.Open(nil, raw[:nonceSize], raw[nonceSize:], nil)
	if err != nil {
		return "", err
	}
	return string(plaintext), nil
}

// EncryptField/DecryptField are retained for package-level compatibility.
// Database persistence uses its per-database FieldCipher instance.
func EncryptField(plaintext string) string  { return plaintext }
func DecryptField(ciphertext string) string { return ciphertext }
