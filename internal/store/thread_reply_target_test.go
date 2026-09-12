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
			want := "<root@example.test> <second@example.test> <third@example.test>"
			if target.References != want {
				t.Fatalf("references = %q, want %q", target.References, want)
			}
		})

		t.Run("parent with no chain", func(t *testing.T) {
			threadID := inbound("<only@example.test>", "<parent@example.test>", nil, now)
			target, err := st.GetThreadReplyTarget(ctx, inboxID, threadID)
			if err != nil {
				t.Fatal(err)
			}
			if target.References != "<parent@example.test> <only@example.test>" {
				t.Fatalf("references = %q", target.References)
			}
		})

		t.Run("no ancestors at all", func(t *testing.T) {
			threadID := inbound("<first@example.test>", "", nil, now)
			target, err := st.GetThreadReplyTarget(ctx, inboxID, threadID)
			if err != nil {
				t.Fatal(err)
			}
			if target.References != "<first@example.test>" || target.InReplyTo != "<first@example.test>" {
				t.Fatalf("target = %+v", target)
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
			want := "<root@example.test> <second@example.test> <good@example.test>"
			if target.References != want {
				t.Fatalf("references = %q, want %q", target.References, want)
			}
		})

		// References grows one ID per hop, so it is bounded by dropping the
		// oldest rather than emitting an unbounded header.
		t.Run("bounded chain", func(t *testing.T) {
			ancestors := make([]string, 0, 400)
			for index := range 400 {
				ancestors = append(ancestors, "<"+strings.Repeat("x", 40)+uuid.NewString()+"-"+string(rune('a'+index%26))+"@example.test>")
			}
			threadID := inbound("<newest@example.test>", "", ancestors, now)
			target, err := st.GetThreadReplyTarget(ctx, inboxID, threadID)
			if err != nil {
				t.Fatal(err)
			}
			if len(target.References) > maxReferencesBytes {
				t.Fatalf("references is %d bytes, over the %d bound", len(target.References), maxReferencesBytes)
			}
			if !strings.HasSuffix(target.References, "<newest@example.test>") {
				t.Fatal("the newest ancestor was dropped instead of the oldest")
			}
		})
	})
}
