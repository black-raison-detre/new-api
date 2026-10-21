package openrouter

import openrouterdto "github.com/QuantumNous/new-api/relay/channel/openrouter/dto"

type RequestReasoning struct {
	Enabled bool `json:"enabled"`
	// One of the following (not both):
	Effort    string `json:"effort,omitempty"`     // Can be "high", "medium", or "low" (OpenAI-style)
	MaxTokens int    `json:"max_tokens,omitempty"` // Specific token limit (Anthropic-style)
	// Optional: Default is false. All models support this.
	Exclude bool `json:"exclude,omitempty"` // Set to true to exclude reasoning tokens from response
}

type OpenRouterEnterpriseResponse = openrouterdto.EnterpriseResponse
