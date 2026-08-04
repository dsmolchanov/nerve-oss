package httpsafe

import (
	"errors"
	"net/http"
	"testing"
	"time"
)

func TestValidateDialAddress(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		address string
		unsafe  bool
	}{
		{name: "public IPv4", address: "93.184.216.34:443"},
		{name: "public IPv6", address: "[2606:2800:220:1:248:1893:25c8:1946]:443"},
		{name: "loopback IPv4", address: "127.0.0.1:80", unsafe: true},
		{name: "loopback IPv6", address: "[::1]:80", unsafe: true},
		{name: "RFC1918", address: "10.1.2.3:80", unsafe: true},
		{name: "carrier grade NAT", address: "100.64.0.1:80", unsafe: true},
		{name: "link local metadata", address: "169.254.169.254:80", unsafe: true},
		{name: "IPv4 protocol assignments", address: "192.0.0.170:80", unsafe: true},
		{name: "IPv4 documentation", address: "192.0.2.1:80", unsafe: true},
		{name: "IPv4 benchmarking", address: "198.18.0.1:80", unsafe: true},
		{name: "IPv4 reserved", address: "240.0.0.1:80", unsafe: true},
		{name: "IPv6 link local", address: "[fe80::1]:80", unsafe: true},
		{name: "IPv6 unique local", address: "[fc00::1]:80", unsafe: true},
		{name: "IPv6 deprecated site local", address: "[fec0::1]:80", unsafe: true},
		{name: "IPv6 NAT64", address: "[64:ff9b::1]:80", unsafe: true},
		{name: "IPv6 discard only", address: "[100::1]:80", unsafe: true},
		{name: "IPv6 protocol assignments", address: "[2001::1]:80", unsafe: true},
		{name: "IPv6 documentation legacy", address: "[2001:db8::1]:80", unsafe: true},
		{name: "IPv6 6to4", address: "[2002::1]:80", unsafe: true},
		{name: "IPv6 documentation", address: "[3fff::1]:80", unsafe: true},
		{name: "unspecified", address: "0.0.0.0:80", unsafe: true},
		{name: "multicast", address: "224.0.0.1:80", unsafe: true},
		{name: "IPv4 mapped loopback", address: "[::ffff:127.0.0.1]:80", unsafe: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateDialAddress(test.address)
			if test.unsafe && !errors.Is(err, ErrUnsafeAddress) {
				t.Fatalf("expected unsafe-address error, got %v", err)
			}
			if !test.unsafe && err != nil {
				t.Fatalf("expected address to be allowed, got %v", err)
			}
		})
	}
}

func TestClientRejectsHostnameResolvingToLoopbackAtDialTime(t *testing.T) {
	t.Parallel()

	client, err := New(Config{Timeout: time.Second, AllowedHosts: []string{"localhost"}})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	request, err := http.NewRequest(http.MethodGet, "http://localhost:80", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	_, err = client.Do(request)
	if !errors.Is(err, ErrUnsafeAddress) {
		t.Fatalf("expected dial-time unsafe-address error, got %v", err)
	}
}

func TestClientRejectsRedirects(t *testing.T) {
	t.Parallel()

	client, err := New(Config{Timeout: time.Second})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	redirect, err := http.NewRequest(http.MethodGet, "http://169.254.169.254/latest", nil)
	if err != nil {
		t.Fatalf("new redirect request: %v", err)
	}
	if err := client.CheckRedirect(redirect, nil); !errors.Is(err, http.ErrUseLastResponse) {
		t.Fatalf("expected redirects to be refused, got %v", err)
	}
}

func TestClientRequiresTimeoutAndValidAllowlist(t *testing.T) {
	t.Parallel()

	if _, err := New(Config{}); err == nil {
		t.Fatal("expected zero timeout to fail")
	}
	if _, err := New(Config{Timeout: time.Second, AllowedHosts: []string{"example.com:443"}}); err == nil {
		t.Fatal("expected host with port in allowlist to fail")
	}
}

func TestClientDisablesEnvironmentProxyBypass(t *testing.T) {
	t.Parallel()

	client, err := New(Config{Timeout: time.Second})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	allowlistTransport, ok := client.Transport.(*hostAllowlistTransport)
	if !ok {
		t.Fatalf("unexpected outer transport %T", client.Transport)
	}
	transport, ok := allowlistTransport.next.(*http.Transport)
	if !ok {
		t.Fatalf("unexpected inner transport %T", allowlistTransport.next)
	}
	if transport.Proxy != nil {
		t.Fatal("environment proxy remained enabled")
	}
}
