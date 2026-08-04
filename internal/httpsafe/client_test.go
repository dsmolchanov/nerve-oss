package httpsafe

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"net/http"
	"sync"
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

func TestClientRejectsHostnameResolvingToPrivateAddressAtDialTime(t *testing.T) {
	resolver, _ := newControlledResolver(t, [4]byte{10, 1, 2, 3})
	client, err := New(Config{
		Timeout:      time.Second,
		AllowedHosts: []string{"private.test"},
		Resolver:     resolver,
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	_, err = client.Get("http://private.test/")
	if !errors.Is(err, ErrUnsafeAddress) {
		t.Fatalf("expected controlled 10/8 resolution to fail at dial time, got %v", err)
	}
}

func TestClientRejectsPublicToPrivateDNSRebindAtDialTime(t *testing.T) {
	resolver, setAddress := newControlledResolver(t, [4]byte{93, 184, 216, 34})
	resolved, err := resolver.LookupHost(context.Background(), "rebind.test")
	if err != nil {
		t.Fatalf("initial public lookup: %v", err)
	}
	if len(resolved) != 1 || resolved[0] != "93.184.216.34" {
		t.Fatalf("initial lookup=%v, want public fixture address", resolved)
	}

	client, err := New(Config{
		Timeout:      time.Second,
		AllowedHosts: []string{"rebind.test"},
		Resolver:     resolver,
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	validatedTransport, ok := client.Transport.(*hostAllowlistTransport)
	if !ok {
		t.Fatalf("unexpected transport %T", client.Transport)
	}
	dialTransport := validatedTransport.next
	validatedTransport.next = roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		// hostAllowlistTransport invokes next only after the production URL and
		// host validation has passed. Rebind at that exact boundary so the
		// production dialer, rather than the preflight lookup above, receives
		// the private answer.
		setAddress([4]byte{10, 20, 30, 40})
		return dialTransport.RoundTrip(request)
	})
	_, err = client.Get("http://rebind.test/")
	if !errors.Is(err, ErrUnsafeAddress) {
		t.Fatalf("expected private rebound address to fail at dial time, got %v", err)
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

// newControlledResolver serves one mutable A record over a real UDP DNS
// socket. That exercises net.Resolver and net.Dialer together instead of
// calling the address validator directly, including a public-to-private
// answer change between validation and the actual client dial.
func newControlledResolver(t *testing.T, initial [4]byte) (*net.Resolver, func([4]byte)) {
	t.Helper()
	packetConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen DNS fixture: %v", err)
	}
	t.Cleanup(func() { _ = packetConn.Close() })

	var addressMu sync.RWMutex
	address := initial
	setAddress := func(next [4]byte) {
		addressMu.Lock()
		address = next
		addressMu.Unlock()
	}

	go func() {
		buffer := make([]byte, 512)
		for {
			read, peer, readErr := packetConn.ReadFrom(buffer)
			if readErr != nil {
				return
			}
			addressMu.RLock()
			current := address
			addressMu.RUnlock()
			response, responseErr := controlledDNSResponse(buffer[:read], current)
			if responseErr == nil {
				_, _ = packetConn.WriteTo(response, peer)
			}
		}
	}()

	resolver := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "udp", packetConn.LocalAddr().String())
		},
	}
	return resolver, setAddress
}

func controlledDNSResponse(query []byte, address [4]byte) ([]byte, error) {
	if len(query) < 17 {
		return nil, errors.New("short DNS query")
	}
	questionEnd := 12
	for {
		if questionEnd >= len(query) {
			return nil, errors.New("invalid DNS question")
		}
		labelLength := int(query[questionEnd])
		questionEnd++
		if labelLength == 0 {
			break
		}
		questionEnd += labelLength
	}
	if questionEnd+4 > len(query) {
		return nil, errors.New("truncated DNS question")
	}
	questionEnd += 4
	questionType := binary.BigEndian.Uint16(query[questionEnd-4 : questionEnd-2])
	answerCount := uint16(0)
	if questionType == 1 {
		answerCount = 1
	}

	response := make([]byte, 12, questionEnd+16)
	copy(response[0:2], query[0:2])
	binary.BigEndian.PutUint16(response[2:4], 0x8180)
	binary.BigEndian.PutUint16(response[4:6], 1)
	binary.BigEndian.PutUint16(response[6:8], answerCount)
	response = append(response, query[12:questionEnd]...)
	if answerCount == 0 {
		return response, nil
	}
	response = append(response,
		0xc0, 0x0c, // compressed owner name
		0x00, 0x01, // A
		0x00, 0x01, // IN
		0x00, 0x00, 0x00, 0x3c, // TTL 60
		0x00, 0x04,
		address[0], address[1], address[2], address[3],
	)
	return response, nil
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}
