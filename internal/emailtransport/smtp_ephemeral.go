package emailtransport

import (
	"neuralmail/internal/domains"
)

// decryptSMTPPassword decrypts an AES-GCM encrypted SMTP password.
func decryptSMTPPassword(ciphertext string, key []byte) (string, error) {
	return domains.DecryptDKIMKey(ciphertext, key)
}
