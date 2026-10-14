package model

import "fmt"

type APIError struct {
	Status int
	Detail any
}

func (e *APIError) Error() string         { return fmt.Sprint(e.Detail) }
func Err(status int, detail string) error { return &APIError{status, detail} }
