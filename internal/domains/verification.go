package domains

import (
	"context"
	"net"
	"strings"
)

const OwnershipTXTLabel = "_nerve-verify"

type TXTResolver interface {
	LookupTXT(ctx context.Context, name string) ([]string, error)
}

type netTXTResolver struct {
	r *net.Resolver
}

func (n netTXTResolver) LookupTXT(ctx context.Context, name string) ([]string, error) {
	return n.r.LookupTXT(ctx, name)
}

type OwnershipVerification struct {
	Verified bool
	Details  string
}

type Verifier struct {
	Resolver TXTResolver
}

func NewVerifier(resolver TXTResolver) *Verifier {
	if resolver == nil {
		resolver = netTXTResolver{r: net.DefaultResolver}
	}
	return &Verifier{Resolver: resolver}
}

func ownershipFQDN(domain string) string {
	return OwnershipTXTLabel + "." + strings.TrimSuffix(domain, ".")
}

func containsTXT(records []string, needle string) bool {
	for _, rec := range records {
		if strings.Contains(rec, needle) {
			return true
		}
	}
	return false
}

func (v *Verifier) VerifyOwnership(ctx context.Context, domain string, verificationToken string) OwnershipVerification {
	// Primary location: _nerve-verify.<domain>
	name := ownershipFQDN(domain)
	records, err := v.Resolver.LookupTXT(ctx, name)
	if err == nil {
		if containsTXT(records, verificationToken) {
			return OwnershipVerification{Verified: true, Details: "found TXT token at " + name}
		}
		return OwnershipVerification{Verified: false, Details: "TXT record exists at " + name + " but token not found"}
	}

	// Fallback: allow the TXT token to exist at the root of the domain.
	records, err2 := v.Resolver.LookupTXT(ctx, domain)
	if err2 == nil {
		if containsTXT(records, verificationToken) {
			return OwnershipVerification{Verified: true, Details: "found TXT token at " + domain}
		}
		return OwnershipVerification{Verified: false, Details: "TXT record exists at " + domain + " but token not found"}
	}

	// Prefer the more actionable error.
	if dnsErr, ok := err.(*net.DNSError); ok && dnsErr.IsNotFound {
		if dnsErr2, ok2 := err2.(*net.DNSError); ok2 && dnsErr2.IsNotFound {
			return OwnershipVerification{Verified: false, Details: "TXT record not found at " + name}
		}
		return OwnershipVerification{Verified: false, Details: "TXT record not found at " + name}
	}
	if dnsErr2, ok2 := err2.(*net.DNSError); ok2 && dnsErr2.IsNotFound {
		return OwnershipVerification{Verified: false, Details: "TXT record not found at " + name}
	}

	return OwnershipVerification{Verified: false, Details: "DNS lookup failed"}
}

