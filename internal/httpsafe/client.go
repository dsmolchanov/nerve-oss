// Package httpsafe builds HTTP clients that reject non-public dial targets.
// The check runs for every resolved address immediately before connect, so a
// hostname cannot pass validation and then rebind to an internal address.
package httpsafe

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"syscall"
	"time"
)

var ErrUnsafeAddress = errors.New("unsafe network address")

// specialUsePrefixes covers non-public ranges that Go deliberately still
// classifies as global unicast. A webhook transport must be stricter than
// IsGlobalUnicast: carrier-grade NAT, benchmarking, documentation,
// translation/tunnelling and reserved ranges may be internally routed or map
// to a non-public destination.
var specialUsePrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("100::/64"),
	netip.MustParsePrefix("2001::/23"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("3fff::/20"),
	netip.MustParsePrefix("5f00::/16"),
	netip.MustParsePrefix("fec0::/10"),
}

type Config struct {
	Timeout      time.Duration
	AllowedHosts []string
	Resolver     *net.Resolver
}

func New(config Config) (*http.Client, error) {
	if config.Timeout <= 0 {
		return nil, errors.New("httpsafe timeout must be positive")
	}

	allowedHosts := make(map[string]struct{}, len(config.AllowedHosts))
	for _, host := range config.AllowedHosts {
		normalized := strings.ToLower(strings.TrimSpace(host))
		if normalized == "" || strings.Contains(normalized, ":") {
			return nil, fmt.Errorf("invalid allowed host %q", host)
		}
		allowedHosts[normalized] = struct{}{}
	}

	dialer := &net.Dialer{
		Timeout:   config.Timeout,
		KeepAlive: 30 * time.Second,
		Resolver:  config.Resolver,
		ControlContext: func(_ context.Context, _, address string, _ syscall.RawConn) error {
			return validateDialAddress(address)
		},
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// Environment proxies can resolve and fetch the request target themselves,
	// bypassing this process's dial-time address check.
	transport.Proxy = nil
	transport.DialContext = dialer.DialContext

	return &http.Client{
		Timeout: config.Timeout,
		Transport: &hostAllowlistTransport{
			allowedHosts: allowedHosts,
			next:         transport,
		},
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}, nil
}

type hostAllowlistTransport struct {
	allowedHosts map[string]struct{}
	next         http.RoundTripper
}

func (t *hostAllowlistTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request == nil || request.URL == nil {
		return nil, errors.New("missing request URL")
	}
	if request.URL.Scheme != "http" && request.URL.Scheme != "https" {
		return nil, fmt.Errorf("unsupported URL scheme %q", request.URL.Scheme)
	}
	host := strings.ToLower(strings.TrimSpace(request.URL.Hostname()))
	if host == "" {
		return nil, errors.New("missing request host")
	}
	if len(t.allowedHosts) > 0 {
		if _, ok := t.allowedHosts[host]; !ok {
			return nil, fmt.Errorf("host %q is not allowed", host)
		}
	}
	return t.next.RoundTrip(request)
}

func validateDialAddress(address string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("parse dial address: %w", err)
	}
	addr, err := netip.ParseAddr(strings.Trim(host, "[]"))
	if err != nil {
		return fmt.Errorf("%w: unresolved address %q", ErrUnsafeAddress, host)
	}
	addr = addr.Unmap()
	if addr.IsLoopback() || addr.IsPrivate() || addr.IsLinkLocalUnicast() ||
		addr.IsUnspecified() || addr.IsMulticast() || !addr.IsGlobalUnicast() ||
		isSpecialUse(addr) {
		return fmt.Errorf("%w: %s", ErrUnsafeAddress, addr.String())
	}
	return nil
}

func isSpecialUse(addr netip.Addr) bool {
	for _, prefix := range specialUsePrefixes {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}
