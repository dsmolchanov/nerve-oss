package domains

import (
	"errors"
	"strings"
	"testing"
)

func TestCanonicalizeDomain(t *testing.T) {
	tests := []struct {
		input   string
		want    string
		wantErr bool
	}{
		{input: "acme.com", want: "acme.com"},
		{input: "ACME.COM", want: "acme.com"},
		{input: "Acme.Com.", want: "acme.com"},
		{input: "  acme.com  ", want: "acme.com"},
		{input: "sub.acme.com", want: "sub.acme.com"},
		{input: "SUB.ACME.COM.", want: "sub.acme.com"},
		{input: "my-domain.co.uk", want: "my-domain.co.uk"},
		{input: "bücher.example", want: "xn--bcher-kva.example"},
		{input: "BÜCHER.Example.", want: "xn--bcher-kva.example"},
		{input: "XN--BCHER-KVA.EXAMPLE", want: "xn--bcher-kva.example"},
		{input: "例え.テスト", want: "xn--r8jz45g.xn--zckzah"},
		{input: "XN--R8JZ45G.XN--ZCKZAH", want: "xn--r8jz45g.xn--zckzah"},

		// Invalid cases
		{input: "", wantErr: true},
		{input: "   ", wantErr: true},
		{input: "https://acme.com", wantErr: true},
		{input: "acme.com/path", wantErr: true},
		{input: "not a domain", wantErr: true},
		{input: "localhost", wantErr: true},    // no TLD
		{input: "-invalid.com", wantErr: true}, // starts with dash
		{input: "invalid-.com", wantErr: true}, // ends with dash
		{input: "inv alid.com", wantErr: true}, // spaces
		{input: "acme.c", wantErr: true},       // TLD too short
		{input: ".acme.com", wantErr: true},    // starts with dot
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			got, err := CanonicalizeDomain(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q, got %q", tc.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for %q: %v", tc.input, err)
			}
			if got != tc.want {
				t.Fatalf("CanonicalizeDomain(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestCanonicalizeDomainRejectsIDNThatExceedsDNSLabelLimit(t *testing.T) {
	if got, err := CanonicalizeDomain(strings.Repeat("é", 64) + ".example"); err == nil {
		t.Fatalf("oversized IDN label canonicalized to %q", got)
	}
}

func TestIsExactProviderResourceID(t *testing.T) {
	for _, value := range []string{"rd-a", "a"} {
		if !IsExactProviderResourceID(value, len(value)) {
			t.Fatalf("valid provider identity %q rejected", value)
		}
	}
	for _, value := range []string{"", " rd-a", "rd-a ", "rd/a", `rd\\a`, "rd?a", "rd#a"} {
		if IsExactProviderResourceID(value, 256) {
			t.Fatalf("invalid provider identity %q accepted", value)
		}
	}
	if IsExactProviderResourceID("rd-a", 0) || IsExactProviderResourceID("rd-a", 3) {
		t.Fatal("invalid provider identity bound accepted")
	}
}

func TestCanonicalizeFreeApexRequiresExactICANNRegistrableDomain(t *testing.T) {
	for _, test := range []struct{ input, want string }{
		{"Example.COM", "example.com"}, {" example.com. ", "example.com"},
		{"Example.Co.Uk.", "example.co.uk"},
		{"BÜCHER.DE", "xn--bcher-kva.de"}, {"XN--BCHER-KVA.DE.", "xn--bcher-kva.de"},
		{"пример.рф", "xn--e1afmkfd.xn--p1ai"},
	} {
		t.Run(test.input, func(t *testing.T) {
			got, err := CanonicalizeFreeApex(test.input)
			if err != nil || got != test.want {
				t.Fatalf("got %q %v want %q", got, err, test.want)
			}
			replay, err := CanonicalizeFreeApex(got)
			if err != nil || replay != got {
				t.Fatalf("canonical pool key not idempotent: %q %v", replay, err)
			}
		})
	}
	for _, input := range []string{
		"", "com", "co.uk", "mail.example.com", "mail.example.co.uk",
		"github.io", "example.github.io", "foo.blogspot.com", "example.unknownsuffix",
		"bücher.example", "example.localhost", "127.0.0.1", "192.168.0.100",
		"２００.１００.１００.１００", "[2001:db8::1]", "https://example.com", "example.com/path",
		"*.example.com", "example..com", "-example.com", "example.com:443",
	} {
		t.Run(input, func(t *testing.T) {
			got, err := CanonicalizeFreeApex(input)
			if got != "" || !errors.Is(err, ErrFreeApexRequired) {
				t.Fatalf("non-apex got %q %v", got, err)
			}
		})
	}
	// The Free contract must not narrow paid domain onboarding.
	if got, err := CanonicalizeDomain("mail.example.com"); err != nil || got != "mail.example.com" {
		t.Fatalf("paid hostname changed: %q %v", got, err)
	}
}
