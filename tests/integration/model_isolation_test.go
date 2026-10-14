//go:build linux

package integration

import (
	"encoding/json"
	"errors"
	"fmt"
)

// Hash only candidates with the test credential's public shape. Never place
// the actual token in a tool command, model prompt or retained evidence file.
func modelIsolationProbe(digest, destination string) string {
	return `expected='` + digest + `'
credential_found=false
anthropic_env_found=false
bridge_enabled=false
[[ ${PWNMESH_MODEL_BRIDGE-} == dispatcher-v1 ]] && bridge_enabled=true
processes_checked=0
files_checked=0
jobs_checked=0
events_checked=0
model_files_checked=0
scan_file() {
  local candidate sum
  while IFS= read -r candidate; do
    sum=$(printf '%s' "$candidate" | sha256sum)
    [[ ${sum%% *} == "$expected" ]] && credential_found=true
  done < <(grep -aEo 'audit-[[:xdigit:]]{64}-token' "$1" 2>/dev/null || true)
}
for file in /proc/[0-9]*/environ; do
  [[ -r $file ]] || continue
  processes_checked=$((processes_checked + 1))
  while IFS= read -r -d '' entry; do
    [[ ${entry%%=*} == ANTHROPIC_* ]] && anthropic_env_found=true
  done < "$file" 2>/dev/null
  scan_file "$file"
done
while IFS= read -r -d '' file; do
  files_checked=$((files_checked + 1))
  case ${file##*/} in
    job.json) jobs_checked=$((jobs_checked + 1));;
    events.jsonl) events_checked=$((events_checked + 1));;
    model-*.json) model_files_checked=$((model_files_checked + 1));;
  esac
  scan_file "$file"
done < <(find /workspace -type f -print0)
printf '{"credential_found":%s,"anthropic_env_found":%s,"bridge_enabled":%s,"processes_checked":%s,"files_checked":%s,"jobs_checked":%s,"events_checked":%s,"model_files_checked":%s}\n' "$credential_found" "$anthropic_env_found" "$bridge_enabled" "$processes_checked" "$files_checked" "$jobs_checked" "$events_checked" "$model_files_checked" | tee '` + destination + `'
`
}

func checkModelIsolationReceipt(text string, requireBridge bool) error {
	var result struct {
		CredentialFound   bool `json:"credential_found"`
		AnthropicEnvFound bool `json:"anthropic_env_found"`
		BridgeEnabled     bool `json:"bridge_enabled"`
		ProcessesChecked  int  `json:"processes_checked"`
		FilesChecked      int  `json:"files_checked"`
		JobsChecked       int  `json:"jobs_checked"`
		EventsChecked     int  `json:"events_checked"`
		ModelFilesChecked int  `json:"model_files_checked"`
	}
	if err := json.Unmarshal([]byte(text), &result); err != nil {
		return errors.New("isolation probe did not return a JSON receipt")
	}
	if result.CredentialFound || result.AnthropicEnvFound || requireBridge && !result.BridgeEnabled {
		return errors.New("provider credential or environment reached the Worker")
	}
	if result.ProcessesChecked < 1 || result.FilesChecked < 1 || result.JobsChecked < 1 || result.EventsChecked < 1 || result.ModelFilesChecked < 1 {
		return fmt.Errorf("isolation probe omitted required process or retained-file categories: %+v", result)
	}
	return nil
}
