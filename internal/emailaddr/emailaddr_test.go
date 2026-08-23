package emailaddr

import (
	"strings"
	"testing"
)

func TestCanonicalizePreservesConservativeASCIIDomainSemantics(t *testing.T) {
	canonical, localPart, domain, err := Canonicalize("  Agent+Tag@EXAMPLE.COM.  ")
	if err != nil {
		t.Fatalf("Canonicalize ASCII address: %v", err)
	}
	if canonical != "agent+tag@example.com" || localPart != "agent+tag" || domain != "example.com" {
		t.Fatalf("Canonicalize ASCII address = (%q, %q, %q), want canonical components", canonical, localPart, domain)
	}
	canonical, _, _, err = Canonicalize("\u00a0\u2003Agent+Tag@EXAMPLE.COM.\u3000")
	if err != nil || canonical != "agent+tag@example.com" {
		t.Fatalf("Canonicalize legacy boundary whitespace = %q, %v", canonical, err)
	}

	for _, address := range []string{
		"agent@bücher.example",
		"agent@例え.テスト",
		"K@example.com",
		"agent@K.example",
		"ſ@example.com",
		"agent@ſ.example",
	} {
		if _, _, _, err := Canonicalize(address); err == nil {
			t.Fatalf("Canonicalize(%q) accepted non-ASCII inbox domain", address)
		}
	}

	for _, domain := range []string{"K.example", "ſ.example", "bücher.example"} {
		if _, err := CanonicalizeDomain(domain); err == nil {
			t.Fatalf("CanonicalizeDomain(%q) accepted non-ASCII lower-level input", domain)
		}
	}
}

func TestCanonicalizeAcceptsASCIIALabelDomain(t *testing.T) {
	canonical, localPart, domain, err := Canonicalize("Agent@XN--R8JZ45G.XN--ZCKZAH.")
	if err != nil {
		t.Fatalf("Canonicalize A-label address: %v", err)
	}
	if canonical != "agent@xn--r8jz45g.xn--zckzah" || localPart != "agent" || domain != "xn--r8jz45g.xn--zckzah" {
		t.Fatalf("Canonicalize A-label address = (%q, %q, %q), want canonical components", canonical, localPart, domain)
	}
	if _, _, _, err := Canonicalize("agent@xn--a.example"); err == nil {
		t.Fatal("Canonicalize accepted structurally plausible but invalid A-label")
	}
}

func TestCanonicalizeDomainDNSBounds(t *testing.T) {
	for _, tc := range []struct {
		name    string
		length  int
		wantErr bool
	}{
		{name: "label N-1", length: 62},
		{name: "label N", length: 63},
		{name: "label N+1", length: 64, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			domain := strings.Repeat("a", tc.length) + ".com"
			got, err := CanonicalizeDomain(domain)
			if tc.wantErr && err == nil {
				t.Fatalf("CanonicalizeDomain(%q) = %q, want error", domain, got)
			}
			if !tc.wantErr && (err != nil || got != domain) {
				t.Fatalf("CanonicalizeDomain(%q) = %q, %v", domain, got, err)
			}
		})
	}

	for _, tc := range []struct {
		name       string
		lastLength int
		wantLength int
		wantErr    bool
	}{
		{name: "domain N-1", lastLength: 60, wantLength: 252},
		{name: "domain N", lastLength: 61, wantLength: 253},
		{name: "domain N+1", lastLength: 62, wantLength: 254, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			domain := strings.Join([]string{
				strings.Repeat("a", 63),
				strings.Repeat("b", 63),
				strings.Repeat("c", 63),
				strings.Repeat("d", tc.lastLength),
			}, ".")
			if len(domain) != tc.wantLength {
				t.Fatalf("fixture length = %d, want %d", len(domain), tc.wantLength)
			}
			got, err := CanonicalizeDomain(domain + ".")
			if tc.wantErr && err == nil {
				t.Fatalf("CanonicalizeDomain(%q) = %q, want error", domain, got)
			}
			if !tc.wantErr && (err != nil || got != domain) {
				t.Fatalf("CanonicalizeDomain(%q) = %q, %v", domain, got, err)
			}
		})
	}

	if got, err := CanonicalizeDomain("a..example"); err == nil {
		t.Fatalf("CanonicalizeDomain with empty label = %q, want error", got)
	}
}
