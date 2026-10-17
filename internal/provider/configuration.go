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
// settings. getenv selects the process or isolated worker environment.
func FromEnvironment(getenv func(string) string, effort string) (*Client, error) {
	protocol := getenv("PWNMESH_MODEL_PROTOCOL")
	if protocol == "" {
		protocol = "anthropic"
	}
	if protocol != "anthropic" && protocol != "openai" {
		return nil, errors.New("model protocol must be anthropic or openai")
	}
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
	timeout, err := positive("PWNMESH_REQUEST_TIMEOUT", int(DefaultRequestTimeout/time.Second))
	if err != nil {
		return nil, err
	}
	if int64(timeout) > int64((1<<63-1)/time.Second) {
		return nil, errors.New("PWNMESH_REQUEST_TIMEOUT exceeds the supported duration")
	}
	p := &Client{
		Protocol: protocol,
		BaseURL:  getenv("ANTHROPIC_BASE_URL"), Token: getenv("ANTHROPIC_AUTH_TOKEN"), Model: getenv("ANTHROPIC_MODEL"),
		ReasoningEffort: effort, MaxTokens: output, Timeout: time.Duration(timeout) * time.Second,
	}
	if p.Model == "" {
		p.Model = getenv("ANTHROPIC_DEFAULT_FABLE_MODEL")
	}
	tokenKey := "ANTHROPIC_AUTH_TOKEN"
	if protocol == "openai" {
		p.BaseURL, p.Model, p.Token = getenv("OPENAI_BASE_URL"), getenv("OPENAI_MODEL"), getenv("OPENAI_API_KEY")
		if p.BaseURL == "" {
			p.BaseURL = DefaultOpenAIBaseURL
		}
		if strings.TrimSpace(p.Model) == "" {
			return nil, errors.New("OPENAI_MODEL is required")
		}
		tokenKey = "OPENAI_API_KEY"
	}
	if strings.TrimSpace(p.Token) == "" {
		return nil, fmt.Errorf("%s is required", tokenKey)
	}
	p.Client, err = connectionClient(getenv("PWNMESH_CONNECTION_MODE"), getenv("PWNMESH_PROXY_URL"))
	if err != nil {
		return nil, err
	}
	return p, nil
}
