package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

const (
	encryptionKeyEnv  = "ENCRYPTION_KEY"
	encryptionKeyFile = "encryption.key"
	keySize           = 32 // AES-256
)

// LoadOrGenerateKey loads the encryption key from env var, file, or generates a new one.
// Priority: ENCRYPTION_KEY env var > data_dir/encryption.key file > auto-generate.
func LoadOrGenerateKey(dataDir string, logger *slog.Logger) ([]byte, error) {
	// 1. Try env var
	if raw := os.Getenv(encryptionKeyEnv); raw != "" {
		key, err := hex.DecodeString(strings.TrimSpace(raw))
		if err != nil || len(key) != keySize {
			return nil, fmt.Errorf("ENCRYPTION_KEY must be %d hex-encoded bytes (got %d)", keySize, len(key))
		}
		logger.Info("encryption key loaded from env var")
		return key, nil
	}

	// 2. Try key file
	keyPath := filepath.Join(dataDir, encryptionKeyFile)
	if data, err := os.ReadFile(keyPath); err == nil {
		key, err := hex.DecodeString(strings.TrimSpace(string(data)))
		if err != nil || len(key) != keySize {
			return nil, fmt.Errorf("encryption key file %s is corrupted", keyPath)
		}
		logger.Info("encryption key loaded from file", "path", keyPath)
		return key, nil
	}

	// 3. Auto-generate
	key := make([]byte, keySize)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, fmt.Errorf("generating encryption key: %w", err)
	}

	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return nil, fmt.Errorf("creating data directory: %w", err)
	}
	if err := os.WriteFile(keyPath, []byte(hex.EncodeToString(key)+"\n"), 0600); err != nil {
		return nil, fmt.Errorf("saving encryption key: %w", err)
	}

	logger.Warn("encryption key auto-generated — back up this file",
		"path", keyPath,
		"hint", "set ENCRYPTION_KEY env var for explicit control",
	)
	return key, nil
}

// Encrypt encrypts plaintext using AES-256-GCM. Returns base64-encoded ciphertext.
func Encrypt(plaintext string, key []byte) (string, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("creating cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("creating GCM: %w", err)
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("generating nonce: %w", err)
	}

	ciphertext := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(ciphertext), nil
}

// Decrypt decrypts base64-encoded AES-256-GCM ciphertext.
func Decrypt(encoded string, key []byte) (string, error) {
	ciphertext, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("decoding base64: %w", err)
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("creating cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("creating GCM: %w", err)
	}

	nonceSize := gcm.NonceSize()
	if len(ciphertext) < nonceSize {
		return "", fmt.Errorf("ciphertext too short")
	}

	nonce, ciphertext := ciphertext[:nonceSize], ciphertext[nonceSize:]
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", fmt.Errorf("decrypting: %w", err)
	}

	return string(plaintext), nil
}
