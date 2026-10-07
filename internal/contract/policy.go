package contract

import (
	"encoding/json"
	"errors"
	"fmt"
)

// Policy is bound to the registered job, never selected by model output.
type Policy struct {
	Version  int
	GraphRPC bool
}

func ParseWithPolicy(output, kind string, conclude bool, policy Policy) (Result, error) {
	if policy.Version != 2 || !policy.GraphRPC {
		return Result{}, errors.New("execution requires result contract 2 and a live graph bridge")
	}
	if kind != "reason" && kind != "curate" && kind != "explore" {
		return Result{}, fmt.Errorf("unsupported task kind %q", kind)
	}
	if conclude && kind != "explore" {
		return Result{}, errors.New("control roles have no conclusion phase")
	}
	m, err := Extract(output)
	if err != nil {
		return Result{}, err
	}
	var accepted bool
	if raw := m["accepted"]; len(raw) == 0 || string(raw) == "null" || json.Unmarshal(raw, &accepted) != nil {
		return Result{}, errors.New("accepted must be true or false")
	}
	if !accepted {
		reason, err := text(m["reason"])
		if err != nil || len(m) != 2 {
			return Result{}, errors.New("rejection requires only accepted:false and a nonempty reason")
		}
		return Result{Kind: "rejected", Reason: reason}, nil
	}
	if kind != "explore" {
		key, result := "decided", "decided"
		if kind == "curate" {
			key, result = "curated", "curated"
		}
		data, err := object(m["data"])
		if err != nil || len(m) != 2 || len(data) != 1 || string(data[key]) != "true" {
			return Result{}, fmt.Errorf("%s success requires only accepted:true and data containing %s:true; a server receipt must authorize it", kind, key)
		}
		return Result{Kind: result}, nil
	}
	outcome, err := text(m["outcome"])
	if err != nil {
		return Result{}, errors.New("outcome must be completed, continue or incomplete")
	}
	switch outcome {
	case "continue", "incomplete":
		reason, err := text(m["reason"])
		if err != nil || len(m) != 3 {
			return Result{}, errors.New("continue/incomplete requires only accepted, outcome and a nonempty reason")
		}
		if outcome == "continue" && conclude {
			return Result{}, errors.New("continue is unavailable during conclusion; report completed or incomplete")
		}
		return Result{Kind: outcome, Outcome: outcome, Reason: reason}, nil
	case "completed":
		if len(m) != 3 || m["data"] == nil {
			return Result{}, errors.New("completed requires only accepted, outcome and data")
		}
		return parseEvidenceResult(m["data"])
	default:
		return Result{}, fmt.Errorf("unknown execution outcome %q", outcome)
	}
}
