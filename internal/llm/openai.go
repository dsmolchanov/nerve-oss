package llm

import (
	"context"
	"errors"
)

type OpenAI struct {
	APIKey string
	Model  string
}

func NewOpenAI(apiKey string, model string) *OpenAI {
	if model == "" {
		model = "gpt-4o-mini"
	}
	return &OpenAI{APIKey: apiKey, Model: model}
}

func (o *OpenAI) Name() string { return "openai" }
func (o *OpenAI) Model() string { return o.Model }

func (o *OpenAI) Classify(_ context.Context, _ string, _ map[string]any) (Classification, error) {
	return Classification{}, errors.New("openai provider not implemented")
}

func (o *OpenAI) Extract(_ context.Context, _ string, _ map[string]any, _ []map[string]any) (Extraction, error) {
	return Extraction{}, errors.New("openai provider not implemented")
}

func (o *OpenAI) Draft(_ context.Context, _ string, _ map[string]any, _ string) (Draft, error) {
	return Draft{}, errors.New("openai provider not implemented")
}
