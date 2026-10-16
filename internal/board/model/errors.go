package model

import "fmt"

type APIError struct {
	Status int
	Detail any
	Code   string
}

func (e *APIError) Error() string         { return fmt.Sprint(e.Detail) }
func Err(status int, detail string) error { return &APIError{Status: status, Detail: detail} }

const (
	CodeStateChanged          = "state_changed"
	CodeDependencyInvalidated = "dependency_invalidated"
	CodeRecoveryExhausted     = "recovery_exhausted"
)

func CodedErr(status int, code, detail string) error {
	return &APIError{Status: status, Code: code, Detail: detail}
}
