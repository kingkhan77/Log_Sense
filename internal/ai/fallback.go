package ai

import (
	"context"
	"fmt"
	"strings"
)

type FallbackSummarizer struct {
	providers []Summarizer
}

var _ Summarizer = (*FallbackSummarizer)(nil)

func NewFallbackSummarizer(providers ...Summarizer) Summarizer {
	filtered := make([]Summarizer, 0, len(providers))
	for _, p := range providers {
		if p != nil {
			filtered = append(filtered, p)
		}
	}
	if len(filtered) == 0 {
		return nil
	}
	if len(filtered) == 1 {
		return filtered[0]
	}
	return &FallbackSummarizer{providers: filtered}
}

func (f *FallbackSummarizer) Provider() string {
	names := make([]string, 0, len(f.providers))
	for _, p := range f.providers {
		names = append(names, p.Provider())
	}
	return "fallback(" + strings.Join(names, " -> ") + ")"
}

func (f *FallbackSummarizer) SummarizeIncident(ctx context.Context, serviceName, incidentType string, rawContext map[string]any) (string, error) {
	var errs []string
	for _, p := range f.providers {
		summary, err := p.SummarizeIncident(ctx, serviceName, incidentType, rawContext)
		if err == nil {
			return summary, nil
		}
		errs = append(errs, p.Provider()+": "+err.Error())
	}
	return "", fmt.Errorf("all providers failed: %s", strings.Join(errs, "; "))
}
