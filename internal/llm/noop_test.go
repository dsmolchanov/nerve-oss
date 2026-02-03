package llm

import (
	"context"
	"testing"
)

func TestNoopTriageUrgentNegative(t *testing.T) {
	provider := NewNoop()
	res, err := provider.Classify(context.Background(), "Critical server outage and angry refund", nil)
	if err != nil {
		t.Fatalf("classify error: %v", err)
	}
	if res.Urgency != "high" {
		t.Fatalf("expected high urgency, got %s", res.Urgency)
	}
	if res.Sentiment != "negative" {
		t.Fatalf("expected negative sentiment, got %s", res.Sentiment)
	}
}
