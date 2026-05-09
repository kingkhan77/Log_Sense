package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// OpenAIClient implements Summarizer using GPT-4o-mini.
// It satisfies the Summarizer interface — the compiler enforces this.
type OpenAIClient struct {
	apiKey string
	model  string
	http   *http.Client
}

// Compile-time check: if OpenAIClient ever stops satisfying Summarizer,
// this line fails to compile with a clear error. Much better than finding
// out at runtime.
var _ Summarizer = (*OpenAIClient)(nil)

func NewOpenAIClient() (*OpenAIClient, error) {
	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		return nil, fmt.Errorf("OPENAI_API_KEY is not set")
	}
	return &OpenAIClient{
		apiKey: key,
		model:  "gpt-4o-mini",
		// 30s timeout on the HTTP client — OpenAI can be slow under load.
		// We don't want the worker goroutine to hang indefinitely waiting
		// for a response that may never come.
		http: &http.Client{Timeout: 30 * time.Second},
	}, nil
}

// SummarizeIncident builds a structured prompt from the incident's raw_context
// and asks GPT-4o-mini to produce a plain-English ops summary.
//
// Prompt design decisions:
//  1. We give the model a clear role ("on-call engineer") so it produces
//     practical, not academic, output.
//  2. We ask for exactly three things: what happened, impact, recommended action.
//     Open-ended prompts produce unfocused responses.
//  3. We include all numeric context (error_rate, p95, threshold, window) so
//     the model can give specific, not generic, advice.
//  4. We cap output at ~150 words via the prompt instruction — we're storing
//     this in a text column and displaying it in a dashboard, not writing a novel.
// Provider implements Summarizer.
func (c *OpenAIClient) Provider() string { return "openai" }

// SummarizeIncident implements Summarizer.
func (c *OpenAIClient) SummarizeIncident(ctx context.Context, serviceName, incidentType string, rawContext map[string]any) (string, error) {
	// Serialize raw_context to a readable key: value block for the prompt.
	// We build it manually rather than json.Marshal so the model sees
	// "error_rate: 1.00" not {"error_rate":1} — more natural to parse.
	var contextLines strings.Builder
	for k, v := range rawContext {
		contextLines.WriteString(fmt.Sprintf("  %s: %v\n", k, v))
	}

	prompt := fmt.Sprintf(`You are an on-call engineer reviewing a backend incident alert.

Incident details:
  service: %s
  type: %s
  context:
%s
Write a concise incident summary (max 3 sentences, ~100 words) covering:
1. What happened and which threshold was breached
2. The likely operational impact
3. The recommended immediate action

Be specific — use the numbers from the context. Do not be generic.`,
		serviceName,
		incidentType,
		contextLines.String(),
	)

	return c.complete(ctx, prompt)
}

// --- internal types ---

type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatResponse struct {
	Choices []struct {
		Message chatMessage `json:"message"`
	} `json:"choices"`
}

// complete sends a single user message to the chat completions endpoint
// and returns the model's text response.
func (c *OpenAIClient) complete(ctx context.Context, prompt string) (string, error) {
	payload := chatRequest{
		Model:    c.model,
		Messages: []chatMessage{{Role: "user", Content: prompt}},
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://api.openai.com/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("http call: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("openai returned status %d", resp.StatusCode)
	}

	var result chatResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}
	if len(result.Choices) == 0 {
		return "", fmt.Errorf("no choices returned")
	}

	return result.Choices[0].Message.Content, nil
}
