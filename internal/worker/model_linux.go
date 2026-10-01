//go:build linux

package worker

import (
	"errors"
	"os"
	"slices"
	"strings"
	"time"

	"pwnmesh/internal/config"
	"pwnmesh/internal/provider"
)

func modelForJob(j Job) (*provider.Anthropic, error) {
	effort := j.Budget.ReasoningEffort
	if effort == "" {
		effort = config.Getenv("PWNMESH_REASONING_EFFORT")
	}
	if effort == "" {
		effort = provider.DefaultReasoningEffort
	}
	if !slices.Contains([]string{"low", "high", "max"}, effort) {
		return nil, errors.New("reasoning effort must be low, high or max")
	}
	p := &provider.Anthropic{
		BaseURL: os.Getenv("ANTHROPIC_BASE_URL"), Token: os.Getenv("ANTHROPIC_AUTH_TOKEN"),
		Model: os.Getenv("ANTHROPIC_MODEL"), SessionID: j.RunID,
		MaxTokens:       envInt("PWNMESH_MAX_OUTPUT_TOKENS", provider.DefaultMaxTokens),
		ReasoningEffort: effort, Timeout: time.Duration(envInt("PWNMESH_REQUEST_TIMEOUT", 180)) * time.Second,
	}
	if p.Model == "" {
		p.Model = os.Getenv("ANTHROPIC_DEFAULT_FABLE_MODEL")
	}
	if strings.TrimSpace(p.Token) == "" {
		return nil, errors.New("ANTHROPIC_AUTH_TOKEN is required")
	}
	return p, nil
}
