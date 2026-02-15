package domains

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
)

// EncryptDKIMKey encrypts a PEM-encoded private key using AES-256-GCM.
// The encryptionKey must be exactly 32 bytes. The ciphertext is returned
// as base64 for storage in the dkim_private_key_enc column.
func EncryptDKIMKey(plainPEM string, encryptionKey []byte) (string, error) {
	if len(encryptionKey) != 32 {
		return "", fmt.Errorf("encryption key must be 32 bytes, got %d", len(encryptionKey))
	}
	block, err := aes.NewCipher(encryptionKey)
	if err != nil {
		return "", fmt.Errorf("create cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("create GCM: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("generate nonce: %w", err)
	}
	ciphertext := gcm.Seal(nonce, nonce, []byte(plainPEM), nil)
	return base64.StdEncoding.EncodeToString(ciphertext), nil
}

// DecryptDKIMKey decrypts an AES-GCM encrypted DKIM private key.
// The ciphertext is expected to be base64-encoded (as stored in DB).
func DecryptDKIMKey(ciphertext string, encryptionKey []byte) (string, error) {
	if len(encryptionKey) != 32 {
		return "", fmt.Errorf("encryption key must be 32 bytes, got %d", len(encryptionKey))
	}
	data, err := base64.StdEncoding.DecodeString(ciphertext)
	if err != nil {
		return "", fmt.Errorf("decode base64: %w", err)
	}
	block, err := aes.NewCipher(encryptionKey)
	if err != nil {
		return "", fmt.Errorf("create cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("create GCM: %w", err)
	}
	nonceSize := gcm.NonceSize()
	if len(data) < nonceSize {
		return "", errors.New("ciphertext too short")
	}
	nonce, ciphertextBytes := data[:nonceSize], data[nonceSize:]
	plaintext, err := gcm.Open(nil, nonce, ciphertextBytes, nil)
	if err != nil {
		return "", fmt.Errorf("decrypt: %w", err)
	}
	return string(plaintext), nil
}
