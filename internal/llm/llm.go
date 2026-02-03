package llm

import (
	"context"
)

type Classification struct {
	Intent    string
	Urgency   string
	Sentiment string
	Confidence float64
}

type Extraction struct {
	Data           map[string]any
	Confidence     float64
	MissingFields  []string
	ValidationErrors []string
}

type Draft struct {
	Text          string
	Citations     []string
	RiskFlags     []string
	NeedsApproval bool
}

type Provider interface {
	Classify(ctx context.Context, text string, taxonomy map[string]any) (Classification, error)
	Extract(ctx context.Context, text string, schema map[string]any, examples []map[string]any) (Extraction, error)
	Draft(ctx context.Context, contextText string, policy map[string]any, goal string) (Draft, error)
	Name() string
	Model() string
}
