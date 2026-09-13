package emailtransport

import (
	"strings"
	"testing"
)

// References carries the ancestors and the reply target is appended here, once.
// A store read that also included the target would emit it twice, and a client
// reading a duplicated chain cannot tell where the conversation forked. Every
// provider — SMTP, Resend, hybrid — sees whatever this produces.
func TestThreadingHeadersNameTheReplyTargetExactlyOnce(t *testing.T) {
	cases := map[string]struct {
		inReplyTo, references string
		want                  map[string]string
	}{
		"ancestors and target": {
			inReplyTo:  "<third@example.test>",
			references: "<root@example.test> <second@example.test>",
			want: map[string]string{
				"In-Reply-To": "<third@example.test>",
				"References":  "<root@example.test> <second@example.test> <third@example.test>",
			},
		},
		"target with no ancestors": {
			inReplyTo: "<first@example.test>",
			want: map[string]string{
				"In-Reply-To": "<first@example.test>",
				"References":  "<first@example.test>",
			},
		},
		// A first message in a conversation has no parent, and must carry no
		// threading headers rather than empty ones.
		"no target":               {want: nil},
		"ancestors but no target": {references: "<root@example.test>", want: nil},
		// The ancestors were copied from inbound mail, so a sender can include
		// the message's own identifier, or repeat one. Neither may reach the
		// header twice.
		"ancestors already name the target": {
			inReplyTo:  "<third@example.test>",
			references: "<root@example.test> <third@example.test> <second@example.test>",
			want: map[string]string{
				"In-Reply-To": "<third@example.test>",
				"References":  "<root@example.test> <second@example.test> <third@example.test>",
			},
		},
		"repeated ancestors": {
			inReplyTo:  "<third@example.test>",
			references: "<root@example.test> <root@example.test> <second@example.test> <root@example.test>",
			want: map[string]string{
				"In-Reply-To": "<third@example.test>",
				"References":  "<root@example.test> <second@example.test> <third@example.test>",
			},
		},
		"ancestors are only the target": {
			inReplyTo:  "<only@example.test>",
			references: "<only@example.test> <only@example.test>",
			want: map[string]string{
				"In-Reply-To": "<only@example.test>",
				"References":  "<only@example.test>",
			},
		},
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			got := threadingHeaders(input.inReplyTo, input.references)
			if len(got) != len(input.want) {
				t.Fatalf("headers = %v, want %v", got, input.want)
			}
			for header, value := range input.want {
				if got[header] != value {
					t.Fatalf("%s = %q, want %q", header, got[header], value)
				}
			}
			if target := got["In-Reply-To"]; target != "" {
				if count := strings.Count(got["References"], target); count != 1 {
					t.Fatalf("the reply target appears %d times in References %q", count, got["References"])
				}
			}
		})
	}
}
