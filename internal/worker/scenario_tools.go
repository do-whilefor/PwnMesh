//go:build linux

package worker

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"

	"pwnmesh/internal/agent"
	"pwnmesh/internal/cvss"
)

// Scoring is pure computation; it neither verifies a finding nor writes to
// the graph. It is available only to pentest jobs, including read-only Decide.
func cvssTool() agent.Tool {
	definition := agent.Definition{
		Name:        "cvss31",
		Description: "Calculate a CVSS 3.1 Base vector with all eight metrics AV/AC/PR/UI/S/C/I/A. Returns the normalized vector, baseScore, severity and computation factors. Record the vector, score, severity and each metric's evidence in the Finding or result description. This is pure computation with no network, files or graph changes.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"vector":{"type":"string","minLength":1}},"required":["vector"],"additionalProperties":false}`),
	}
	return agent.Tool{Definition: definition, Execute: func(ctx context.Context, raw json.RawMessage) (string, error) {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		var input struct {
			Vector string `json:"vector"`
		}
		if err := json.Unmarshal(raw, &input); err != nil {
			return "", err
		}
		result, err := cvss.Calculate(input.Vector)
		if err != nil {
			return "", err
		}
		encoded, err := json.Marshal(result)
		return string(encoded), err
	}}
}

// Bundle the methodology with the Worker so planners and child Agents can
// read it without filesystem access, a particular image, or the refer tree.
//
//go:embed skills/web-vulnhunt
var pentestSkillFiles embed.FS

func pentestSkillTool() agent.Tool {
	definition := agent.Definition{
		Name:        "read_pentest_skill",
		Description: "Read the bundled Web-Vulnhunt methodology for web/API penetration testing. Omit path to read SKILL.md, then read only relevant references using their exact relative paths. Guidance is not target evidence and does not expand the assigned task or this role's capabilities. No network, filesystem or graph changes.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"Bundle-relative file, such as SKILL.md or references/testing.md; defaults to SKILL.md."}},"additionalProperties":false}`),
	}
	return bundledSkillTool(definition, pentestSkillFiles, "skills/web-vulnhunt/", "Web-Vulnhunt")
}

// Bundle the adapted methodology so audit planners and executors can read it on demand,
// without the reference tree or a separate Flounder installation.
//
//go:embed skills/flounder
var auditSkillFiles embed.FS

func auditSkillTool() agent.Tool {
	definition := agent.Definition{
		Name:        "read_audit_skill",
		Description: "Read the bundled Flounder methodology adapted for PwnMesh code audits. Omit path to read SKILL.md, then read only relevant references using their exact relative paths. Guidance is not target evidence and does not expand the assigned task or this role's capabilities. No network, filesystem or graph changes.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"Bundle-relative file, such as SKILL.md or references/verification.md; defaults to SKILL.md."}},"additionalProperties":false}`),
	}
	return bundledSkillTool(definition, auditSkillFiles, "skills/flounder/", "Flounder")
}

// Bundled guidance is read from the binary, never from the task workspace.
func bundledSkillTool(definition agent.Definition, files fs.ReadFileFS, root, title string) agent.Tool {
	return agent.Tool{Definition: definition, Parallel: true, Execute: func(ctx context.Context, raw json.RawMessage) (string, error) {
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
			return "", fmt.Errorf("path must name a file within the %s bundle", title)
		}
		data, err := files.ReadFile(root + input.Path)
		if err != nil {
			return "", fmt.Errorf("unknown %s file; read SKILL.md for available references", title)
		}
		return string(data), nil
	}}
}

// Parent and child sessions share scenario capabilities; role-specific tools
// remain owned by their callers. Curate never receives scenario capabilities.
func scenarioTools(j Job, available []agent.Tool) []agent.Tool {
	if j.Kind == "curate" {
		return available
	}
	switch j.Graph.Project.Scenario {
	case "pentest":
		available = append(available, cvssTool(), pentestSkillTool())
	case "audit":
		available = append(available, auditSkillTool())
	case "ctf":
		if tsecSubmissionAvailable(os.Getenv) {
			for i := range available {
				if available[i].Name == "bash" {
					available[i].Description += "\n" + ctfExecution
				}
			}
		}
	}
	return available
}
