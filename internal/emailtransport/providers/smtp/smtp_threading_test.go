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
	// The longest References and In-Reply-To the derivation can produce.
	identifier := func(size int) string {
		return "<" + strings.Repeat("a", size-2) + ">"
	}
	budget := 998 - len("References: ")
	ancestors := make([]string, 0, 32)
	for index := 0; len(strings.Join(ancestors, " ")) < budget; index++ {
		// Distinct identifiers: the worker deduplicates, so repeats would not
		// reach the wire and the line would be shorter than intended.
		ancestors = append(ancestors, "<"+strings.Repeat("a", 30)+strings.Repeat("b", index%7+1)+"-"+identifierIndex(index)+">")
	}
	references := strings.Join(ancestors, " ")
	// Trim to the budget the way the derivation does, leaving room for the
	// reply target the worker appends.
	parent := identifier(40)
	for len(references)+1+len(parent) > budget {
		ancestors = ancestors[1:]
		references = strings.Join(ancestors, " ")
	}

	// The longest identifier a line can carry, and one byte less, to pin the
	// boundary rather than a round number.
	longest := identifier(998 - len("In-Reply-To: "))
	cases := map[string]map[string]string{
		"long chain": {
			"In-Reply-To": parent,
			"References":  references + " " + parent,
		},
		"longest single identifier": {
			"In-Reply-To": longest,
			"References":  identifier(998 - len("References: ")),
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
			// The adapter emits CRLF; RFC 5322's 998 counts the line without
			// its terminator, and SMTP's 1000 octets counts it with.
			for _, line := range strings.Split(raw, "\n") {
				line = strings.TrimSuffix(line, "\r")
				if len(line) > 998 {
					t.Fatalf("line of %d characters exceeds RFC 5322's limit: %.80s…", len(line), line)
				}
				if len(line)+2 > 1000 {
					t.Fatalf("line of %d octets with CRLF exceeds SMTP's DATA limit", len(line)+2)
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

// identifierIndex keeps generated identifiers distinct.
func identifierIndex(index int) string {
	return strings.Repeat("c", index/26+1) + string(rune('a'+index%26))
}
