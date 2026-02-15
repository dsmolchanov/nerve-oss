package domains

import (
	"crypto/rand"
	"strings"
	"testing"
)

func TestEncryptDecryptRoundTrip(t *testing.T) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("generate key: %v", err)
	}

	plainPEM := `-----BEGIN RSA PRIVATE KEY-----
MIIBogIBAAJBALRiMLAEv+kJV7MHhjP7mbJEMEk3bGF8EXAMPLE
-----END RSA PRIVATE KEY-----`

	ciphertext, err := EncryptDKIMKey(plainPEM, key)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if ciphertext == "" {
		t.Fatal("ciphertext is empty")
	}
	if ciphertext == plainPEM {
		t.Fatal("ciphertext equals plaintext")
	}

	decrypted, err := DecryptDKIMKey(ciphertext, key)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if decrypted != plainPEM {
		t.Fatalf("round-trip failed: got %q, want %q", decrypted, plainPEM)
	}
}

func TestEncryptRejectsWrongKeySize(t *testing.T) {
	_, err := EncryptDKIMKey("data", make([]byte, 16))
	if err == nil {
		t.Fatal("expected error for 16-byte key")
	}
}

func TestDecryptRejectsWrongKey(t *testing.T) {
	key1 := make([]byte, 32)
	key2 := make([]byte, 32)
	rand.Read(key1)
	rand.Read(key2)

	ct, err := EncryptDKIMKey("secret", key1)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	_, err = DecryptDKIMKey(ct, key2)
	if err == nil {
		t.Fatal("expected decryption to fail with wrong key")
	}
}

func TestDecryptRejectsInvalidBase64(t *testing.T) {
	key := make([]byte, 32)
	rand.Read(key)
	_, err := DecryptDKIMKey("not-base64!!!", key)
	if err == nil {
		t.Fatal("expected error for invalid base64")
	}
}

func TestDecryptRejectsTruncatedCiphertext(t *testing.T) {
	key := make([]byte, 32)
	rand.Read(key)
	_, err := DecryptDKIMKey("YQ==", key)
	if err == nil {
		t.Fatal("expected error for truncated ciphertext")
	}
}

func TestEncryptProducesDifferentCiphertexts(t *testing.T) {
	key := make([]byte, 32)
	rand.Read(key)
	ct1, _ := EncryptDKIMKey("same", key)
	ct2, _ := EncryptDKIMKey("same", key)
	if ct1 == ct2 {
		t.Fatal("expected different ciphertexts due to random nonce")
	}
}

func TestEncryptLargeKey(t *testing.T) {
	key := make([]byte, 32)
	rand.Read(key)
	largePEM := strings.Repeat("A", 4096)
	ct, err := EncryptDKIMKey(largePEM, key)
	if err != nil {
		t.Fatalf("encrypt large key: %v", err)
	}
	dec, err := DecryptDKIMKey(ct, key)
	if err != nil {
		t.Fatalf("decrypt large key: %v", err)
	}
	if dec != largePEM {
		t.Fatal("round-trip failed for large key")
	}
}
