package provider

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// FromEnvironment keeps execution and readiness checks on the same model
// settings. getenv must resolve any legacy environment aliases first.
func FromEnvironment(getenv func(string) string, effort string) (*Anthropic, error) {
	if effort == "" {
		effort = getenv("PWNMESH_REASONING_EFFORT")
	}
	if effort == "" {
		effort = DefaultReasoningEffort
	}
	if !slices.Contains([]string{"low", "high", "max"}, effort) {
		return nil, errors.New("reasoning effort must be low, high or max")
	}
	positive := func(key string, fallback int) (int, error) {
		value := getenv(key)
		if value == "" {
			return fallback, nil
		}
		n, err := strconv.Atoi(value)
		if err != nil || n <= 0 {
			return 0, fmt.Errorf("%s must be a positive integer", key)
		}
		return n, nil
	}
	output, err := positive("PWNMESH_MAX_OUTPUT_TOKENS", DefaultMaxTokens)
	if err != nil {
		return nil, err
	}
	timeout, err := positive("PWNMESH_REQUEST_TIMEOUT", 180)
	if err != nil {
		return nil, err
	}
	if int64(timeout) > int64((1<<63-1)/time.Second) {
		return nil, errors.New("PWNMESH_REQUEST_TIMEOUT exceeds the supported duration")
	}
	p := &Anthropic{
		BaseURL: getenv("ANTHROPIC_BASE_URL"), Token: getenv("ANTHROPIC_AUTH_TOKEN"), Model: getenv("ANTHROPIC_MODEL"),
		ReasoningEffort: effort, MaxTokens: output, Timeout: time.Duration(timeout) * time.Second,
	}
	if p.Model == "" {
		p.Model = getenv("ANTHROPIC_DEFAULT_FABLE_MODEL")
	}
	if strings.TrimSpace(p.Token) == "" {
		return nil, errors.New("ANTHROPIC_AUTH_TOKEN is required")
	}
	p.Client, err = connectionClient(getenv("PWNMESH_CONNECTION_MODE"), getenv("PWNMESH_PROXY_URL"))
	if err != nil {
		return nil, err
	}
	return p, nil
}
