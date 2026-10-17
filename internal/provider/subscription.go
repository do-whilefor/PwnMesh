package provider

import (
	"errors"
	"net/url"
	"strings"
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
	switch p.AuthMode {
	case "", "api_key":
		if p.TokenSource != nil {
			return errors.New("subscription token source requires Kimi OAuth")
		}
		return nil
	case "kimi_oauth":
		if p.Protocol != "anthropic" || p.endpoint() != "https://api.kimi.com/coding/v1/messages" {
			return errors.New("Kimi OAuth requires the official Kimi Coding Anthropic endpoint")
		}
	case "glm_coding_plan":
		if p.Protocol != "anthropic" || p.endpoint() != "https://open.bigmodel.cn/api/anthropic/v1/messages" || p.TokenSource != nil {
			return errors.New("GLM Coding Plan requires the official BigModel Anthropic endpoint and API key")
		}
	default:
		return errors.New("unsupported model authentication mode")
	}
	return nil
}

func isKimiCodingEndpoint(endpoint *url.URL) bool {
	return endpoint.Scheme == "https" && endpoint.User == nil && endpoint.RawQuery == "" && endpoint.Fragment == "" &&
		(strings.EqualFold(endpoint.Host, "api.kimi.com") || strings.EqualFold(endpoint.Host, "api.kimi.ai")) &&
		endpoint.Path == "/coding/v1/messages"
}
