//go:build linux

package worker

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"io/fs"

	"pwnmesh/internal/agent"
)

// Bundle the adapted methodology so audit planners and executors can read it on demand,
// without the reference tree or a separate Flounder installation.
//
//go:embed skills/flounder
var auditSkillFiles embed.FS

func auditSkillTool() agent.Tool {
	return agent.Tool{
		Definition: agent.Definition{
			Name:        "read_audit_skill",
			Description: "Read the bundled Flounder methodology adapted for PwnMesh code audits. Omit path to read SKILL.md, then read only relevant references using their exact relative paths. Guidance is not target evidence and does not expand the assigned task or this role's capabilities. No network, filesystem or graph changes.",
			Schema:      json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"Bundle-relative file, such as SKILL.md or references/verification.md; defaults to SKILL.md."}},"additionalProperties":false}`),
		},
		Parallel: true,
		Execute: func(ctx context.Context, raw json.RawMessage) (string, error) {
			if err := ctx.Err(); err != nil {
				return "", err
			}
			var input struct {
				Path string `json:"path"`
			}
			if err := json.Unmarshal(raw, &input); err != nil {
				return "", err
			}
			if input.Path == "" {
				input.Path = "SKILL.md"
			}
			if !fs.ValidPath(input.Path) || input.Path == "." {
				return "", errors.New("path must name a file within the Flounder bundle")
			}
			data, err := auditSkillFiles.ReadFile("skills/flounder/" + input.Path)
			if err != nil {
				return "", errors.New("unknown Flounder file; read SKILL.md for available references")
			}
			return string(data), nil
		},
	}
}
