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

type GeminiClient struct {
	apiKey string
	model  string
	http   *http.Client
}

var _ Summarizer = (*GeminiClient)(nil)

func NewGeminiClient() (*GeminiClient, error) {
	key := os.Getenv("GEMINI_API_KEY")
	if key == "" {
		return nil, fmt.Errorf("GEMINI_API_KEY is not set")
	}

	return &GeminiClient{
		apiKey: key,
		model:  "gemini-2.5-flash-lite",
		http:   &http.Client{Timeout: 30 * time.Second},
	}, nil
}

func (c *GeminiClient) Provider() string { return "gemini" }

func (c *GeminiClient) SummarizeIncident(ctx context.Context, serviceName, incidentType string, rawContext map[string]any) (string, error) {
	var contextLines strings.Builder
	for k, v := range rawContext {
		contextLines.WriteString(fmt.Sprintf("  %s: %v\n", k, v))
	}

	prompt := fmt.Sprintf(`Write an operational incident summary for an on-call engineer.
	Keep response under 80 words. Do NOT explain concepts generally.
	Use only the metrics provided. Focus on: detected issue, operational impact, likely cause, immediate action.
	
	Incident details:
	  service: %s
	  type: %s
	  context:
	%s`, serviceName, incidentType, contextLines.String())

	return c.complete(ctx, prompt)
}

type geminiRequest struct {
	Contents         []geminiContent `json:"contents"`
	GenerationConfig map[string]any  `json:"generationConfig,omitempty"`
}

type geminiContent struct {
	Parts []geminiPart `json:"parts"`
}

type geminiPart struct {
	Text string `json:"text"`
}

type geminiResponse struct {
	Candidates []struct {
		Content struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"content"`
	} `json:"candidates"`
}

func (c *GeminiClient) complete(ctx context.Context, prompt string) (string, error) {
	payload := geminiRequest{
		Contents: []geminiContent{
			{Parts: []geminiPart{{Text: prompt}}},
		},
		GenerationConfig: map[string]any{
			"temperature":     0.2,
			"maxOutputTokens": 120,
		},
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}

	url := fmt.Sprintf(
		"https://generativelanguage.googleapis.com/v1beta/models/%s:generateContent?key=%s",
		c.model,
		c.apiKey,
	)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("http call: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("gemini returned status %d", resp.StatusCode)
	}

	var result geminiResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}

	if len(result.Candidates) == 0 ||
		len(result.Candidates[0].Content.Parts) == 0 ||
		result.Candidates[0].Content.Parts[0].Text == "" {
		return "", fmt.Errorf("no candidates returned")
	}

	return result.Candidates[0].Content.Parts[0].Text, nil
}
