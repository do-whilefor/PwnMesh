package worker

import "strings"

// Scenario names do not establish an available contest submission service.
// Both planning and execution advertise TSEC only when its channel is configured.
func tsecSubmissionAvailable(getenv func(string) string) bool {
	return strings.TrimSpace(getenv("TSEC_SERVER_HOST")) != "" &&
		strings.TrimSpace(getenv("TSEC_AGENT_TOKEN")) != ""
}
