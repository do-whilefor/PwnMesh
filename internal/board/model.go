// Package board owns graph persistence and transactional state changes.
package board

import "pwnmesh/internal/board/model"

// Board operations use the same domain types as storage-free model consumers.
type Settings = model.Settings
type Fact = model.Fact
type Intent = model.Intent
type Hint = model.Hint
type Reason = model.Reason
type Project = model.Project
type Graph = model.Graph
type Summary = model.Summary
type Conclusion = model.Conclusion
type Reopened = model.Reopened
type ExecutionFence = model.ExecutionFence
type APIError = model.APIError

func Ptr(s string) *string                { return model.Ptr(s) }
func Value(s *string) string              { return model.Value(s) }
func Err(status int, detail string) error { return model.Err(status, detail) }
