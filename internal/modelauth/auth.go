// Package modelauth defines authentication policy shared by settings and transport.
package modelauth

import (
	"encoding/hex"
	"errors"
)

// ValidateEndpoint checks the final request URL before credentials are loaded.
// Settings can supply their canonical base plus the protocol path; transports
// supply the resolved URL, including any supported endpoint shorthand.
func ValidateEndpoint(protocol, mode, endpoint string) error {
	switch mode {
	case "", "api_key":
		return nil
	case "kimi_oauth":
		if protocol != "anthropic" || endpoint != "https://api.kimi.com/coding/v1/messages" {
			return errors.New("Kimi OAuth requires the official Kimi Coding Anthropic endpoint")
		}
	case "glm_coding_plan":
		if protocol != "anthropic" || endpoint != "https://open.bigmodel.cn/api/anthropic/v1/messages" {
			return errors.New("GLM Coding Plan requires the official BigModel Anthropic endpoint")
		}
	default:
		return errors.New("unsupported model authentication mode")
	}
	return nil
}

func ValidCredentialID(id string) bool {
	if len(id) != 32 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}
