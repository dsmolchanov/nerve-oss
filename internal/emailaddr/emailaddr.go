package emailaddr

import (
	"fmt"
	"regexp"
	"strings"

	"golang.org/x/net/idna"
)

var localPartRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9._+-]*[a-z0-9])?$`)
var validHostnameLabelRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
var validASCIITLDRE = regexp.MustCompile(`^[a-z]{2,}$`)

const (
	maxDomainBytes = 253
	maxLabelBytes  = 63
)

func isASCII(value string) bool {
	for i := 0; i < len(value); i++ {
		if value[i] >= 0x80 {
			return false
		}
	}
	return true
}

// CanonicalizeDomain normalizes a domain for storage:
// - lowercase
// - trim spaces
// - strip trailing dot
// - validate as a valid hostname (no protocol, no path)
// Returns error if domain is invalid.
func CanonicalizeDomain(domain string) (string, error) {
	// Preserve historical boundary-whitespace normalization, then reject before
	// case-folding. Unicode case mappings can turn confusables such as U+212A
	// KELVIN SIGN into valid ASCII hostname bytes.
	d := strings.TrimSpace(domain)
	if !isASCII(d) {
		return "", fmt.Errorf("domain must contain ASCII only")
	}

	d = strings.ToLower(d)
	d = strings.TrimSuffix(d, ".")

	if d == "" {
		return "", fmt.Errorf("domain is empty")
	}
	if strings.Contains(d, "://") {
		return "", fmt.Errorf("domain must not contain protocol: %q", domain)
	}
	if strings.Contains(d, "/") {
		return "", fmt.Errorf("domain must not contain path: %q", domain)
	}
	if strings.Contains(d, " ") {
		return "", fmt.Errorf("domain must not contain spaces: %q", domain)
	}
	if len(d) > maxDomainBytes {
		return "", fmt.Errorf("domain exceeds %d bytes", maxDomainBytes)
	}

	labels := strings.Split(d, ".")
	if len(labels) < 2 {
		return "", fmt.Errorf("invalid domain: %q", domain)
	}
	for _, label := range labels {
		if len(label) == 0 || len(label) > maxLabelBytes || !validHostnameLabelRE.MatchString(label) {
			return "", fmt.Errorf("invalid domain label in %q", domain)
		}
		if strings.HasPrefix(label, "xn--") {
			canonical, err := idna.Lookup.ToASCII(label)
			if err != nil || canonical != label {
				return "", fmt.Errorf("invalid A-label in %q", domain)
			}
		}
	}
	tld := labels[len(labels)-1]
	if !validASCIITLDRE.MatchString(tld) && !strings.HasPrefix(tld, "xn--") {
		return "", fmt.Errorf("invalid top-level domain in %q", domain)
	}
	return d, nil
}

// Canonicalize parses and normalizes an inbox email address.
//
// We intentionally keep validation conservative (ASCII, no display name, no quoted local part)
// to avoid edge cases in downstream providers.
func Canonicalize(address string) (canonical string, localPart string, domain string, err error) {
	// Preserve historical boundary-whitespace normalization, then enforce the
	// documented ASCII-only address before case-folding, which could otherwise
	// turn Unicode confusables into ASCII.
	raw := strings.TrimSpace(address)
	if !isASCII(raw) {
		return "", "", "", fmt.Errorf("address must contain ASCII only")
	}

	if raw == "" {
		return "", "", "", fmt.Errorf("address is empty")
	}
	if strings.ContainsAny(raw, " \t\r\n") {
		return "", "", "", fmt.Errorf("address must not contain spaces")
	}

	// Lowercase for storage and uniqueness.
	raw = strings.ToLower(raw)

	parts := strings.Split(raw, "@")
	if len(parts) != 2 {
		return "", "", "", fmt.Errorf("invalid address: %q", address)
	}
	localPart = strings.TrimSpace(parts[0])
	domain = strings.TrimSpace(parts[1])
	if localPart == "" || domain == "" {
		return "", "", "", fmt.Errorf("invalid address: %q", address)
	}
	if !localPartRE.MatchString(localPart) {
		return "", "", "", fmt.Errorf("invalid local part: %q", localPart)
	}

	canonicalDomain, err := CanonicalizeDomain(domain)
	if err != nil {
		return "", "", "", err
	}
	domain = canonicalDomain

	return localPart + "@" + domain, localPart, domain, nil
}
