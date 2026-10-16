//go:build linux

// Package dag adapts command and Worker-owned Agent nodes to workergraph.
package dag

import (
	"encoding/json"
	"errors"
)

// Scope binds every graph to one parent execution. Identity must encode the
// parent's existing immutable identity, preserving its canonical field order.
// The directory argument includes evidence handoff's absolute-path resolution.
type Scope struct {
	RunID     string
	RunDir    string
	Workspace string
	// WritePaths is the bound Step's advisory shared-output contract. It never
	// authorizes or sandboxes arbitrary shell operations.
	WritePaths        []string
	SharedWritesBound bool
	Identity          func(runDir string) (json.RawMessage, error)
}

func (s Scope) identity(runDir string) (json.RawMessage, error) {
	if s.Identity == nil {
		return nil, errors.New("graph requires a parent execution identity")
	}
	return s.Identity(runDir)
}
