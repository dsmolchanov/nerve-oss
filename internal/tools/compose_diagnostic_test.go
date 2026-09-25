package tools

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"strings"
	"testing"

	"neuralmail/internal/store"
)

func TestComposeFailureLogUsesOnlyFixedStage(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stage string
		cause error
		want  string
	}{
		{"domain", "enqueue", store.ErrDomainNotVerified, "enqueue_domain_unverified"},
		{"policy", "enqueue", store.ErrOutboxPolicyRevoked, "enqueue_policy_revoked"},
		{"idempotency", "enqueue", store.ErrOutboxIdempotencyConflict, "enqueue_idempotency_conflict"},
		{"database", "enqueue", errors.New("database failure"), "enqueue_other"},
		{"message", "message_write", errors.New("message failure"), "message_write"},
		{"untrusted stage", "SECRET_stage", errors.New("private failure"), "other"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var output bytes.Buffer
			previous := log.Writer()
			log.SetOutput(&output)
			defer log.SetOutput(previous)
			wrapped := fmt.Errorf("%w: SECRET_address@example.test", tc.cause)
			if got := logComposeFailure(tc.stage, wrapped); !errors.Is(got, tc.cause) {
				t.Fatalf("error identity changed: %v", got)
			}
			logged := output.String()
			if !strings.Contains(logged, "compose_email failure stage="+tc.want) ||
				strings.Contains(logged, "SECRET_address") || strings.Contains(logged, "SECRET_stage") ||
				strings.Contains(logged, tc.cause.Error()) {
				t.Fatalf("unsafe or missing stage diagnostic: %q", logged)
			}
		})
	}
}
