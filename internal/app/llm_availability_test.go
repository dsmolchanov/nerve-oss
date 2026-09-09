package app

import (
	"errors"
	"neuralmail/internal/config"
	"neuralmail/internal/llm"
	"testing"
)

func TestSelectedLLMAvailability(t *testing.T) {
	for _, tt := range []struct {
		name, provider, key, url string
		unavailable              bool
	}{
		{"noop", "noop", "", "", true},
		{"unknown", "typo", "", "", true},
		{"missing OpenAI key", "openai", "", "", true},
		{"missing Ollama URL", "ollama", "", "", true},
		{"configured OpenAI", "openai", "test-key", "", false},
		{"configured Ollama", "ollama", "", "http://127.0.0.1:11434", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.LLM.Provider = tt.provider
			cfg.LLM.OpenAIKey = tt.key
			cfg.LLM.OllamaURL = tt.url
			if got := errors.Is(llm.RequireAvailable(selectLLM(cfg)), llm.ErrUnavailable); got != tt.unavailable {
				t.Fatalf("unavailable=%v", got)
			}
		})
	}
}
