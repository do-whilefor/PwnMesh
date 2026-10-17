package provider

import (
	"errors"
	"net/url"
	"strings"

	"pwnmesh/internal/modelauth"
)

// AdaptiveThinking also supplies the worker's credential-free request sizer.
func (p *Client) AdaptiveThinking() bool {
	if p.Protocol == "openai" || p.AuthMode == "glm_coding_plan" {
		return false
	}
	if p.AuthMode == "kimi_oauth" {
		return true
	}
	endpoint, err := url.Parse(p.endpoint())
	return err == nil && (strings.EqualFold(endpoint.Hostname(), "openrouter.ai") || isKimiCodingEndpoint(endpoint))
}

func (p *Client) validateAuthEndpoint() error {
	if err := modelauth.ValidateEndpoint(p.Protocol, p.AuthMode, p.endpoint()); err != nil {
		return err
	}
	if p.TokenSource != nil && p.AuthMode != "kimi_oauth" {
		return errors.New("subscription token source requires Kimi OAuth")
	}
	return nil
}

func isKimiCodingEndpoint(endpoint *url.URL) bool {
	return endpoint.Scheme == "https" && endpoint.User == nil && endpoint.RawQuery == "" && endpoint.Fragment == "" &&
		(strings.EqualFold(endpoint.Host, "api.kimi.com") || strings.EqualFold(endpoint.Host, "api.kimi.ai")) &&
		endpoint.Path == "/coding/v1/messages"
}
