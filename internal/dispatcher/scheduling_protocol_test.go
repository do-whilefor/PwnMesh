package dispatcher

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"pwnmesh/internal/board"
)

func TestSchedulingRejectsIncompatibleServerOnEveryPage(t *testing.T) {
	for _, version := range []string{"missing", "null", "0", "2"} {
		for _, mismatchOffset := range []int{0, 100} {
			t.Run(fmt.Sprintf("version_%s_offset_%d", version, mismatchOffset), func(t *testing.T) {
				s, _, _, graph := automaticRetryFixture(t, 0, "")
				requests := 0
				s.Client.HTTP = &http.Client{Transport: executionQueryTransport(func(r *http.Request) (*http.Response, error) {
					requests++
					if !strings.HasSuffix(r.URL.Path, "/scheduling") || r.URL.Query().Get("protocol_version") != strconv.Itoa(board.ScheduleProtocolVersion) {
						t.Fatalf("unexpected request or missing protocol declaration: %s", r.URL)
					}
					offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
					page := map[string]any{
						"protocol_version": board.ScheduleProtocolVersion,
						"project":          graph.Project, "state_version": "unchanged",
						"steps": []board.ScheduleStep{{ID: "ready", Status: "open", Ready: true}},
					}
					if offset == mismatchOffset {
						// This is the old dual-list response: decoding it used to leave
						// Ready false and silently suppress every available Execute.
						page["intents"] = []board.Intent{{ID: "ready"}}
						page["steps"] = []board.Step{{ID: "ready", Status: "open"}}
						delete(page, "protocol_version")
						if version != "missing" {
							page["protocol_version"] = json.RawMessage(version)
						}
					} else {
						page["next_offset"] = mismatchOffset
					}
					body, err := json.Marshal(page)
					if err != nil {
						t.Fatal(err)
					}
					return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(string(body))), Header: http.Header{}}, nil
				})}
				started, err := s.dispatch(context.Background(), graph.Project.ID)
				if started || err == nil || !strings.Contains(err.Error(), "scheduling protocol mismatch") || !strings.Contains(err.Error(), "upgrade Server and Dispatcher together") {
					t.Fatalf("incompatible projection did not fail explicitly: started=%v err=%v", started, err)
				}
				if requests != 1+mismatchOffset/100 || len(s.running) != 0 || len(s.schedules) != 0 {
					t.Fatalf("incompatible/partial input reached dispatch: requests=%d running=%d schedules=%d", requests, len(s.running), len(s.schedules))
				}
			})
		}
	}
}
