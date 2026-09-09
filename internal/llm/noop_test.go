package llm

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestNoopNeverProducesAIResults(t *testing.T) {
	p := NewNoop()
	c, ce := p.Classify(context.Background(), "Critical outage refund", nil)
	e, ee := p.Extract(context.Background(), "invoice", map[string]any{"required": []any{"amount"}}, nil)
	d, de := p.Draft(context.Background(), "message", nil, "reply")
	for _, err := range []error{ce, ee, de, RequireAvailable(p), RequireAvailable(nil)} {
		if !errors.Is(err, ErrUnavailable) {
			t.Fatalf("want unavailable, got %v", err)
		}
	}
	if c != (Classification{}) || !reflect.DeepEqual(e, Extraction{}) || !reflect.DeepEqual(d, Draft{}) {
		t.Fatalf("fabricated results: %#v %#v %#v", c, e, d)
	}
}
