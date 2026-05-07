package ai

import "context"

// Summarizer is the interface the worker depends on for AI-generated summaries.
//
// Defining an interface here rather than depending directly on *OpenAIClient
// means the worker package has zero knowledge of which AI provider is being
// used. To add Gemini, Anthropic, or a mock for tests — you implement this
// interface. The worker doesn't change at all.
//
// This is the Dependency Inversion Principle: high-level policy (worker)
// depends on an abstraction (Summarizer), not a concrete implementation.
type Summarizer interface {
	// SummarizeIncident takes an incident's context and returns a plain-English
	// summary suitable for storing in the ai_summary column and displaying
	// to an on-call engineer.
	SummarizeIncident(ctx context.Context, serviceName, incidentType string, rawContext map[string]any) (string, error)

	// Provider returns the name of the underlying AI provider.
	// Used in logs so you can tell which provider generated a given summary.
	Provider() string
}