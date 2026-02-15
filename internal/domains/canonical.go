package domains

import (
	"fmt"
	"regexp"
	"strings"
)

var validHostnameRE = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]*[a-z0-9])?\.)+[a-z]{2,}$`)

// CanonicalizeDomain normalizes a domain for storage:
// - lowercase
// - trim spaces
// - strip trailing dot
// - validate as a valid hostname (no protocol, no path)
// Returns error if domain is invalid.
func CanonicalizeDomain(domain string) (string, error) {
	d := strings.TrimSpace(domain)
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
	if !validHostnameRE.MatchString(d) {
		return "", fmt.Errorf("invalid domain: %q", domain)
	}
	return d, nil
}
