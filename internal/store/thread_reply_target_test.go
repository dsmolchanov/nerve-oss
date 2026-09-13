package store

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Threading is derived from inbound mail, so every value here was chosen by
// whoever sent it. A reply that does not thread is a far smaller problem than
// one that cannot be sent, or one carrying headers a sender appended.
// threadingHeaderValue is the References a provider sees: the ancestors this
// read returns, with the reply target the outbox worker appends.
func threadingHeaderValue(target ThreadReplyTarget) string {
	return strings.TrimSpace(target.References + " " + target.InReplyTo)
}

func TestGetThreadReplyTargetDerivesAndSanitizesThreading(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateToLatest(t, ctx, db)
		st := &Store{db: db, q: db}

		orgID, inboxID := uuid.NewString(), uuid.NewString()
		if _, err := db.ExecContext(ctx, `INSERT INTO orgs (id, name) VALUES ($1, 'acme')`, orgID); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx,
			`INSERT INTO inboxes (id, org_id, address, status) VALUES ($1, $2, 'agent@test.example', 'active')`,
			inboxID, orgID); err != nil {
			t.Fatal(err)
		}

		// inbound records one received message in its own thread.
		inbound := func(messageID, inReplyTo string, references []string, at time.Time) string {
			threadID := uuid.NewString()
			if _, err := db.ExecContext(ctx,
				`INSERT INTO threads (id, inbox_id, org_id, subject, status, updated_at) VALUES ($1,$2,$3,'s','open',$4)`,
				threadID, inboxID, orgID, at); err != nil {
				t.Fatal(err)
			}
			var refs any
			if len(references) > 0 {
				refs = references
			}
			messageRowID := uuid.NewString()
			if _, err := db.ExecContext(ctx,
				`INSERT INTO messages (id, inbox_id, org_id, thread_id, direction, subject, text, created_at,
				 provider_message_id, internet_message_id, in_reply_to, "references")
				 VALUES ($1,$2,$3,$4,'inbound','s','t',$5,$6,$7,nullif($8,''),$9)`,
				messageRowID, inboxID, orgID, threadID, at, messageRowID, messageID, inReplyTo, refs); err != nil {
				t.Fatal(err)
			}
			return threadID
		}

		now := time.Now().UTC()

		t.Run("chain then parent", func(t *testing.T) {
			threadID := inbound("<third@example.test>", "<second@example.test>",
				[]string{"<root@example.test>", "<second@example.test>"}, now)
			target, err := st.GetThreadReplyTarget(ctx, inboxID, threadID)
			if err != nil {
				t.Fatal(err)
			}
			if target.InReplyTo != "<third@example.test>" {
				t.Fatalf("in-reply-to = %q, want the newest inbound message", target.InReplyTo)
			}
			// Ancestors only: the outbox worker appends the reply target when
			// it builds the header, so naming it here would emit it twice.
			want := "<root@example.test> <second@example.test>"
			if target.References != want {
				t.Fatalf("references = %q, want the ancestors alone %q", target.References, want)
			}
		})

		t.Run("parent with no chain", func(t *testing.T) {
			threadID := inbound("<only@example.test>", "<parent@example.test>", nil, now)
			target, err := st.GetThreadReplyTarget(ctx, inboxID, threadID)
			if err != nil {
				t.Fatal(err)
			}
			if target.References != "<parent@example.test>" {
				t.Fatalf("references = %q, want the ancestor alone", target.References)
			}
		})

		t.Run("no ancestors at all", func(t *testing.T) {
			threadID := inbound("<first@example.test>", "", nil, now)
			target, err := st.GetThreadReplyTarget(ctx, inboxID, threadID)
			if err != nil {
				t.Fatal(err)
			}
			if target.References != "" || target.InReplyTo != "<first@example.test>" {
				t.Fatalf("target = %+v, want no ancestors and the parent alone", target)
			}
		})

		t.Run("newest inbound message wins", func(t *testing.T) {
			threadID := inbound("<older@example.test>", "", nil, now.Add(-time.Hour))
			if _, err := db.ExecContext(ctx,
				`INSERT INTO messages (id, inbox_id, org_id, thread_id, direction, subject, text, created_at,
				 provider_message_id, internet_message_id)
				 VALUES ($1,$2,$3,$4,'inbound','s','t',$5,$6,'<newer@example.test>')`,
				uuid.NewString(), inboxID, orgID, threadID, now, uuid.NewString()); err != nil {
				t.Fatal(err)
			}
			// An outbound message must not become the parent: a reply threads
			// onto what the correspondent sent, not onto our own last word.
			if _, err := db.ExecContext(ctx,
				`INSERT INTO messages (id, inbox_id, org_id, thread_id, direction, subject, text, created_at,
				 provider_message_id, internet_message_id)
				 VALUES ($1,$2,$3,$4,'outbound','s','t',$5,$6,'<ours@example.test>')`,
				uuid.NewString(), inboxID, orgID, threadID, now.Add(time.Minute), uuid.NewString()); err != nil {
				t.Fatal(err)
			}
			target, err := st.GetThreadReplyTarget(ctx, inboxID, threadID)
			if err != nil {
				t.Fatal(err)
			}
			if target.InReplyTo != "<newer@example.test>" {
				t.Fatalf("in-reply-to = %q", target.InReplyTo)
			}
		})

		// A thread the runtime started has no inbound parent to reference, and
		// a reply must not invent one.
		t.Run("no inbound message", func(t *testing.T) {
			threadID := uuid.NewString()
			if _, err := db.ExecContext(ctx,
				`INSERT INTO threads (id, inbox_id, org_id, subject, status, updated_at) VALUES ($1,$2,$3,'s','open',now())`,
				threadID, inboxID, orgID); err != nil {
				t.Fatal(err)
			}
			target, err := st.GetThreadReplyTarget(ctx, inboxID, threadID)
			if err != nil || target.InReplyTo != "" || target.References != "" {
				t.Fatalf("target = %+v err=%v", target, err)
			}
		})

		// A hostile Message-ID must neither reach a header nor break the reply.
		t.Run("malformed parent yields no threading", func(t *testing.T) {
			for name, messageID := range map[string]string{
				"header injection": "<a@x>\r\nBcc: victim@example.test",
				"bare newline":     "<a@x>\n",
				"no angle addr":    "a@x",
				"embedded space":   "<a @x>",
				"nested delimiter": "<a<b>@x>",
				"oversized":        "<" + strings.Repeat("a", 999) + "@x>",
			} {
				t.Run(name, func(t *testing.T) {
					threadID := inbound(messageID, "", nil, now)
					target, err := st.GetThreadReplyTarget(ctx, inboxID, threadID)
					if err != nil {
						t.Fatal(err)
					}
					if target.InReplyTo != "" || target.References != "" {
						t.Fatalf("a malformed Message-ID reached a header: %+v", target)
					}
				})
			}
		})

		// One malformed ancestor is dropped without losing the rest.
		t.Run("malformed ancestor dropped", func(t *testing.T) {
			threadID := inbound("<good@example.test>", "",
				[]string{"<root@example.test>", "broken", "<second@example.test>"}, now)
			target, err := st.GetThreadReplyTarget(ctx, inboxID, threadID)
			if err != nil {
				t.Fatal(err)
			}
			want := "<root@example.test> <second@example.test>"
			if target.References != want {
				t.Fatalf("references = %q, want %q", target.References, want)
			}
		})

		// References grows one ID per hop, so it is bounded by dropping the
		// oldest. The bound covers the reply target the worker appends, and
		// the assembled header must stay inside a protocol line.
		t.Run("bounded chain", func(t *testing.T) {
			ancestors := make([]string, 0, 400)
			for range 400 {
				ancestors = append(ancestors, "<"+uuid.NewString()+"@example.test>")
			}
			threadID := inbound("<newest@example.test>", "", ancestors, now)
			target, err := st.GetThreadReplyTarget(ctx, inboxID, threadID)
			if err != nil {
				t.Fatal(err)
			}
			// What the worker will actually emit.
			assembled := target.References + " " + target.InReplyTo
			if len(assembled) > maxReferencesBytes {
				t.Fatalf("assembled references is %d bytes, over the %d bound", len(assembled), maxReferencesBytes)
			}
			if len("References: "+assembled) > 998 {
				t.Fatalf("the References line would be %d characters, over RFC 5322's limit",
					len("References: "+assembled))
			}
			// The newest ancestors are the ones a client threads on.
			if !strings.HasSuffix(target.References, ancestors[len(ancestors)-1]) {
				t.Fatal("the newest ancestor was dropped instead of the oldest")
			}
			if strings.Contains(target.References, ancestors[0]) {
				t.Fatal("the oldest ancestor was kept over newer ones")
			}
			// The reply target appears exactly once in the assembled header.
			if strings.Count(assembled, target.InReplyTo) != 1 {
				t.Fatalf("the reply target appears %d times in %q",
					strings.Count(assembled, target.InReplyTo), assembled)
			}
		})

		// A sender can put the message's own identifier in its References, or
		// repeat one. The assembled header must still name the target once.
		t.Run("chain already names the parent", func(t *testing.T) {
			threadID := inbound("<current@example.test>", "",
				[]string{"<root@example.test>", "<current@example.test>"}, now)
			target, err := st.GetThreadReplyTarget(ctx, inboxID, threadID)
			if err != nil {
				t.Fatal(err)
			}
			assembled := threadingHeaderValue(target)
			if strings.Count(assembled, "<current@example.test>") != 1 {
				t.Fatalf("the reply target appears more than once in %q", assembled)
			}
			if !strings.Contains(assembled, "<root@example.test>") {
				t.Fatalf("deduplication dropped a real ancestor: %q", assembled)
			}
		})

		// The limits come from the header line, so an identifier is discarded
		// only when it genuinely cannot be serialized — never to satisfy a
		// round number.
		t.Run("identifier size boundary", func(t *testing.T) {
			for _, size := range []int{3, 100, 513, 700, maxMessageIDBytes} {
				threadID := inbound("<"+strings.Repeat("a", size-2)+">", "", nil, now)
				target, err := st.GetThreadReplyTarget(ctx, inboxID, threadID)
				if err != nil {
					t.Fatal(err)
				}
				if target.InReplyTo == "" {
					t.Fatalf("a valid %d-byte identifier was discarded", size)
				}
				if len("In-Reply-To: "+target.InReplyTo) > headerLineLimit {
					t.Fatalf("a %d-byte identifier produced an over-long line", size)
				}
			}
			// One byte past what a line can carry is refused.
			over := "<" + strings.Repeat("a", maxMessageIDBytes-1) + ">"
			threadID := inbound(over, "", nil, now)
			target, err := st.GetThreadReplyTarget(ctx, inboxID, threadID)
			if err != nil {
				t.Fatal(err)
			}
			if target.InReplyTo != "" {
				t.Fatalf("an identifier that cannot be serialized was kept: %d bytes", len(over))
			}
		})

		// A parent whose own identifier fills the budget leaves no room for
		// ancestors, and must still thread rather than emit an over-long line.
		t.Run("parent fills the budget", func(t *testing.T) {
			long := "<" + strings.Repeat("a", maxMessageIDBytes-2) + ">"
			threadID := inbound(long, "", []string{"<root@example.test>"}, now)
			target, err := st.GetThreadReplyTarget(ctx, inboxID, threadID)
			if err != nil {
				t.Fatal(err)
			}
			assembled := strings.TrimSpace(target.References + " " + target.InReplyTo)
			if len("References: "+assembled) > 998 || len("In-Reply-To: "+target.InReplyTo) > 998 {
				t.Fatalf("a long parent produced an over-long header: %d bytes", len(assembled))
			}
			if target.InReplyTo != long {
				t.Fatalf("in-reply-to = %q", target.InReplyTo)
			}
		})
	})
}
