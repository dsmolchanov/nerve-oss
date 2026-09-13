package smtp

import (
	"strings"
	"testing"

	"neuralmail/internal/emailtransport"
)

// Every header this adapter writes goes out on one unfolded line, and RFC 5322
// limits a line to 998 characters while SMTP limits a DATA line to 1000
// octets. A relay may refuse a message that breaks either, so the threading
// headers a reply now carries must stay inside the budget the store's
// derivation promises.
func TestSMTPThreadingHeadersStayInsideTheLineLimit(t *testing.T) {
	// The longest References and In-Reply-To the derivation can produce:
	// 900 bytes assembled, one identifier of 512.
	identifier := func(size int) string {
		return "<" + strings.Repeat("a", size-2) + ">"
	}
	ancestors := make([]string, 0, 32)
	for len(strings.Join(ancestors, " ")) < 900-64 {
		ancestors = append(ancestors, identifier(40))
	}
	references := strings.Join(ancestors, " ")
	// Trim to the budget the way the derivation does, leaving room for the
	// reply target the worker appends.
	parent := identifier(40)
	for len(references)+1+len(parent) > 900 {
		ancestors = ancestors[1:]
		references = strings.Join(ancestors, " ")
	}

	cases := map[string]map[string]string{
		"long chain": {
			"In-Reply-To": parent,
			"References":  references + " " + parent,
		},
		"longest single identifier": {
			"In-Reply-To": identifier(512),
			"References":  identifier(512),
		},
	}
	for name, headers := range cases {
		t.Run(name, func(t *testing.T) {
			raw, err := buildMIMEMessage(emailtransport.OutboundMessage{
				From: "agent@example.test", To: []string{"recipient@example.test"},
				Subject: "Re: hello", TextBody: "body", Headers: headers,
			})
			if err != nil {
				t.Fatalf("a reply with threading headers could not be built: %v", err)
			}
			for _, line := range strings.Split(normalizeNewlines(raw), "\n") {
				if len(line) > 998 {
					t.Fatalf("line of %d characters exceeds RFC 5322's limit: %.80s…", len(line), line)
				}
			}
			// The headers survived rather than being dropped to fit.
			if !strings.Contains(raw, "In-Reply-To: "+headers["In-Reply-To"]) {
				t.Fatal("In-Reply-To was not emitted")
			}
			if !strings.Contains(raw, "References: "+headers["References"]) {
				t.Fatal("References was not emitted")
			}
		})
	}
}
