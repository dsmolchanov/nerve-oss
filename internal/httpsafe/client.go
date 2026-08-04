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
	"strings"
	"syscall"
	"time"
)

var ErrUnsafeAddress = errors.New("unsafe network address")

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
	ip := net.ParseIP(strings.Trim(host, "[]"))
	if ip == nil {
		return fmt.Errorf("%w: unresolved address %q", ErrUnsafeAddress, host)
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() ||
		!ip.IsGlobalUnicast() {
		return fmt.Errorf("%w: %s", ErrUnsafeAddress, ip.String())
	}
	return nil
}
