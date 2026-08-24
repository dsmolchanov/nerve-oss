package domains

import (
	"strings"

	"golang.org/x/net/idna"

	"neuralmail/internal/emailaddr"
)

var domainLookupProfile = idna.New(
	idna.MapForLookup(),
	idna.BidiRule(),
	idna.VerifyDNSLength(true),
)

// IsExactProviderResourceID reports whether value is a non-empty, bounded,
// path-safe opaque provider identity whose bytes require no normalization.
// Callers must never trim or otherwise rewrite a provider-returned identity
// before using it for an exact-ID mutation.
func IsExactProviderResourceID(value string, maxBytes int) bool {
	return maxBytes > 0 && value != "" && value == strings.TrimSpace(value) &&
		len(value) <= maxBytes && !strings.ContainsAny(value, "/\\?#")
}

// CanonicalizeDomain normalizes a domain for storage.
// Delegates to emailaddr.CanonicalizeDomain to avoid a circular dependency
// (emailaddr is a lower-level utility; domains is a higher-level service).
func CanonicalizeDomain(domain string) (string, error) {
	ascii, err := domainLookupProfile.ToASCII(strings.TrimSpace(domain))
	if err != nil {
		return "", err
	}
	return emailaddr.CanonicalizeDomain(ascii)
}
