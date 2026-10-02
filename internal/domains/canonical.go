package domains

import (
	"errors"
	"net"
	"strings"

	"golang.org/x/net/idna"
	"golang.org/x/net/publicsuffix"

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

// ErrFreeApexRequired distinguishes Free's registrable-apex contract from the
// general hostname contract used by paid domains. A subdomain is never silently
// promoted to its parent: its ownership proof cannot authorize an apex lease.
var ErrFreeApexRequired = errors.New("Free requires an exact ICANN registrable apex")

// CanonicalizeFreeApex uses the pinned x/net public suffix table after IDNA.
// Keep the resulting exact apex as the durable pool key; claim cleanup and a
// future suffix-table update must not erase its allocation history.
func CanonicalizeFreeApex(input string) (string, error) {
	canonical, err := CanonicalizeDomain(input)
	if err != nil || net.ParseIP(canonical) != nil {
		return "", ErrFreeApexRequired
	}
	_, icann := publicsuffix.PublicSuffix(canonical)
	if !icann {
		return "", ErrFreeApexRequired
	}
	apex, err := publicsuffix.EffectiveTLDPlusOne(canonical)
	if err != nil || apex != canonical {
		return "", ErrFreeApexRequired
	}
	return canonical, nil
}
