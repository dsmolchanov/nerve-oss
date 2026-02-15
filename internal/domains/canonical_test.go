package domains

import (
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

		// Invalid cases
		{input: "", wantErr: true},
		{input: "   ", wantErr: true},
		{input: "https://acme.com", wantErr: true},
		{input: "acme.com/path", wantErr: true},
		{input: "not a domain", wantErr: true},
		{input: "localhost", wantErr: true},          // no TLD
		{input: "-invalid.com", wantErr: true},       // starts with dash
		{input: "invalid-.com", wantErr: true},       // ends with dash
		{input: "inv alid.com", wantErr: true},       // spaces
		{input: "acme.c", wantErr: true},             // TLD too short
		{input: ".acme.com", wantErr: true},          // starts with dot
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
