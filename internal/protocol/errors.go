package protocol

import (
	"encoding/json"
	"errors"
	"strings"

	"pwnmesh/internal/board/model"
)

// RPCError retains machine-readable failure identity across HTTP and the
// container bridge. Detail is display text and never controls new recovery.
type RPCError struct {
	Code   string `json:"code"`
	Status int    `json:"status,omitempty"`
	Detail string `json:"detail"`
}

func (e *RPCError) Error() string { return e.Detail }

const CodeRequestFailed = "request_failed"

func GraphFailure(err error) *RPCError {
	var failure *RPCError
	if errors.As(err, &failure) {
		copy := *failure
		copy.Detail = err.Error()
		if copy.Code == "" {
			copy.Code = CodeRequestFailed
		}
		return &copy
	}
	var api *model.APIError
	if errors.As(err, &api) {
		code := api.Code
		if code == "" {
			code = CodeRequestFailed
		}
		return &RPCError{Code: code, Status: api.Status, Detail: api.Error()}
	}
	return &RPCError{Code: CodeRequestFailed, Detail: err.Error()}
}

// LegacyGraphFailure adapts retained pre-code bridge responses only. Current
// responses always carry Failure, including unclassified validation errors.
// Status and a top-level detail prefix are both required for old HTTP errors.
func LegacyGraphFailure(message string) *RPCError {
	failure := &RPCError{Code: CodeRequestFailed, Detail: message}
	detail := message
	if strings.HasPrefix(message, "board HTTP 409: ") {
		var body struct {
			Code   json.RawMessage `json:"code"`
			Detail json.RawMessage `json:"detail"`
		}
		if json.Unmarshal([]byte(strings.TrimPrefix(message, "board HTTP 409: ")), &body) != nil {
			return failure
		}
		// An old bridge may carry a newer HTTP envelope as display text.
		// Its explicit code still wins; only code-free retained data may use
		// the legacy detail prefix, including when the explicit code is invalid.
		if len(body.Code) != 0 {
			var code string
			if json.Unmarshal(body.Code, &code) == nil && code != "" {
				failure.Code, failure.Status = code, 409
			}
			return failure
		}
		if json.Unmarshal(body.Detail, &detail) != nil {
			return failure
		}
	} else if strings.HasPrefix(message, "board HTTP ") {
		return failure
	}
	for _, code := range []string{model.CodeStateChanged, model.CodeDependencyInvalidated} {
		if strings.HasPrefix(detail, code+":") {
			failure.Code, failure.Status = code, 409
		}
	}
	if detail == "Dispatcher recovery allowance exhausted" {
		failure.Code, failure.Status = model.CodeRecoveryExhausted, 409
	}
	return failure
}
