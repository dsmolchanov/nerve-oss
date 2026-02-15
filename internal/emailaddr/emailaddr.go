package emailaddr

import (
	"fmt"
	"regexp"
	"strings"

	"neuralmail/internal/domains"
)

var localPartRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9._+-]*[a-z0-9])?$`)

// Canonicalize parses and normalizes an inbox email address.
//
// We intentionally keep validation conservative (ASCII, no display name, no quoted local part)
// to avoid edge cases in downstream providers.
func Canonicalize(address string) (canonical string, localPart string, domain string, err error) {
	raw := strings.TrimSpace(address)
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

	canonicalDomain, err := domains.CanonicalizeDomain(domain)
	if err != nil {
		return "", "", "", err
	}
	domain = canonicalDomain

	return localPart + "@" + domain, localPart, domain, nil
}

