package llm

import (
	"context"
	"strings"
)

type Noop struct{}

func NewNoop() *Noop {
	return &Noop{}
}

func (n *Noop) Name() string { return "noop" }
func (n *Noop) Model() string { return "noop" }

func (n *Noop) Classify(_ context.Context, text string, _ map[string]any) (Classification, error) {
	lower := strings.ToLower(text)
	urgency := "low"
	sentiment := "neutral"
	intent := "general"
	if strings.Contains(lower, "outage") || strings.Contains(lower, "down") || strings.Contains(lower, "critical") {
		urgency = "high"
		intent = "incident"
	}
	if strings.Contains(lower, "refund") || strings.Contains(lower, "angry") || strings.Contains(lower, "cancel") {
		urgency = "high"
		sentiment = "negative"
		if intent == "general" {
			intent = "refund_request"
		}
	}
	if strings.Contains(lower, "invoice") || strings.Contains(lower, "billing") {
		intent = "billing"
	}
	if strings.Contains(lower, "thanks") || strings.Contains(lower, "great") {
		sentiment = "positive"
	}

	return Classification{
		Intent:     intent,
		Urgency:    urgency,
		Sentiment:  sentiment,
		Confidence: 0.42,
	}, nil
}

func (n *Noop) Extract(_ context.Context, _ string, schema map[string]any, _ []map[string]any) (Extraction, error) {
	required := requiredFields(schema)
	missing := make([]string, len(required))
	copy(missing, required)
	return Extraction{
		Data:          map[string]any{},
		Confidence:    0,
		MissingFields: missing,
	}, nil
}

func (n *Noop) Draft(_ context.Context, contextText string, _ map[string]any, goal string) (Draft, error) {
	text := "Hello,\n\n"
	if goal != "" {
		text += goal + "\n\n"
	}
	text += "We received your message and will follow up shortly.\n\n"
	text += "Context:\n" + truncate(contextText, 240)
	text += "\n\nBest,\nNerve"
	return Draft{
		Text:          text,
		Citations:     nil,
		RiskFlags:     nil,
		NeedsApproval: true,
	}, nil
}

func requiredFields(schema map[string]any) []string {
	requiredRaw, ok := schema["required"]
	if !ok {
		return nil
	}
	items, ok := requiredRaw.([]any)
	if !ok {
		return nil
	}
	var out []string
	for _, item := range items {
		if val, ok := item.(string); ok {
			out = append(out, val)
		}
	}
	return out
}

func truncate(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	return text[:limit] + "..."
}
