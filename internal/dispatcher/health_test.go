package dispatcher

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"pwnmesh/internal/agent"
	"pwnmesh/internal/config"
	"pwnmesh/internal/provider"
)

type healthProvider func(context.Context, []agent.Message, []agent.Definition) (agent.Message, error)

func (p healthProvider) Generate(ctx context.Context, messages []agent.Message, tools []agent.Definition, _ agent.Emit) (agent.Message, error) {
	return p(ctx, messages, tools)
}

func healthCall() agent.Message {
	return agent.Message{Role: "assistant", StopReason: "tool_use", Content: []agent.Block{{Type: "tool_use", ID: "check-1", Name: "readiness_check", Input: json.RawMessage(`{"ready":true}`)}}}
}

func healthReceipt(t *testing.T, messages []agent.Message) agent.Message {
	t.Helper()
	if len(messages) != 3 || messages[1].Content[0].ID != "check-1" || messages[2].Role != "user" || len(messages[2].Content) != 1 || messages[2].Content[0].ToolUseID != "check-1" {
		t.Fatalf("lost the tool round trip: %+v", messages)
	}
	var receipt string
	if err := json.Unmarshal(messages[2].Content[0].Content, &receipt); err != nil {
		t.Fatal(err)
	}
	m := agent.Text("assistant", receipt)
	m.StopReason = "end_turn"
	return m
}

func TestHealthProbesEffectiveRolePoliciesWithBoundedToolRoundTrips(t *testing.T) {
	for _, output := range []int{64, 384000} {
		t.Run(strconv.Itoa(output), func(t *testing.T) {
			calls := 0
			var efforts []string
			endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				var body struct {
					Model        string             `json:"model"`
					MaxTokens    int                `json:"max_tokens"`
					Messages     []agent.Message    `json:"messages"`
					Tools        []agent.Definition `json:"tools"`
					OutputConfig struct {
						Effort string `json:"effort"`
					} `json:"output_config"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if body.Model != "fixture" || body.MaxTokens != min(output, healthMaxTokens) {
					t.Errorf("probe changed model or exceeded output bound: model=%s output=%d", body.Model, body.MaxTokens)
				}
				var m agent.Message
				if calls%2 == 1 {
					efforts = append(efforts, body.OutputConfig.Effort)
					if len(body.Messages) != 1 || len(body.Tools) != 1 || body.Tools[0].Name != "readiness_check" {
						t.Errorf("readiness did not request the protocol tool: %+v", body)
					}
					m = healthCall()
				} else {
					if len(body.Tools) != 0 || body.OutputConfig.Effort != efforts[len(efforts)-1] {
						t.Error("receipt turn changed effort or retained tool execution")
					}
					m = healthReceipt(t, body.Messages)
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(m)
			}))
			defer endpoint.Close()
			outputJSON, _ := json.Marshal(output)
			w := config.Worker{TaskTypes: []string{"reason", "curate", "explore"}, Env: map[string]string{
				"ANTHROPIC_BASE_URL": endpoint.URL, "ANTHROPIC_AUTH_TOKEN": "fixture", "ANTHROPIC_DEFAULT_FABLE_MODEL": "fixture",
				"PWNMESH_REASONING_EFFORT": "max", "PWNMESH_MAX_OUTPUT_TOKENS": string(outputJSON),
			}}
			s := &Scheduler{Config: config.Config{Runtime: config.Runtime{HealthTimeout: 5}, Tasks: config.Tasks{Reason: config.Task{ReasoningEffort: "low"}, Curate: config.Task{ReasoningEffort: "low"}}}}
			if err := s.health(context.Background(), w); err != nil || calls != 4 || len(efforts) != 2 || efforts[0] != "low" || efforts[1] != "max" {
				t.Fatalf("effective policies were not checked once: calls=%d efforts=%v err=%v", calls, efforts, err)
			}
			calls, efforts = 0, nil
			if err := s.health(context.Background(), w, config.Task{ReasoningEffort: "high"}); err != nil || calls != 2 || len(efforts) != 1 || efforts[0] != "high" {
				t.Fatalf("task readiness ignored the persisted job policy: calls=%d efforts=%v err=%v", calls, efforts, err)
			}
		})
	}
}

func TestHealthBlocksKnownIncompatibilityUntilSuccessfulRecheck(t *testing.T) {
	w := config.Worker{Name: "fixture", Type: "go", TaskTypes: []string{"reason"}, MaxRunning: 1}
	s := &Scheduler{Config: config.Config{Workers: []config.Worker{w}}}
	cause := &healthFailure{Kind: "incompatible", Err: errors.New("no tool support")}
	s.CheckHealth = func(context.Context, config.Worker) error { return cause }
	if err := s.Health(context.Background(), true); !errors.Is(err, cause) || s.choose("project", "reason") != nil || !s.nextWake.IsZero() {
		t.Fatalf("known incompatibility was reduced to a short retry: %v", err)
	}
	s.CheckHealth = func(context.Context, config.Worker) error {
		return &healthFailure{Kind: "transient", Err: errors.New("unavailable")}
	}
	_ = s.Health(context.Background(), true)
	if s.incompatible[w.Name] == "" {
		t.Fatal("an inconclusive recheck erased known incompatibility")
	}
	s.CheckHealth = func(context.Context, config.Worker) error { return nil }
	if err := s.Health(context.Background(), true); err != nil || s.choose("project", "reason") == nil || len(s.unhealthy) != 0 || len(s.incompatible) != 0 {
		t.Fatalf("successful recheck did not restore the worker: %v", err)
	}
	for _, kind := range []string{"transient", "unverified"} {
		s.recordHealth(w.Name, &healthFailure{Kind: kind, Err: errors.New("not established")})
		if len(s.incompatible) != 0 || !s.unhealthy[w.Name].After(time.Now()) {
			t.Fatalf("%s was incorrectly declared incompatible", kind)
		}
	}
}

func TestHealthRejectsIncompleteOrIncompatibleProtocolWithoutExtraTurns(t *testing.T) {
	for _, scenario := range []string{"text_only", "wrong_tool", "invalid_arguments", "multiple_calls", "truncated_first", "wrong_receipt", "duplicate_receipt", "extra_call", "truncated_last"} {
		t.Run(scenario, func(t *testing.T) {
			calls := 0
			p := healthProvider(func(_ context.Context, messages []agent.Message, _ []agent.Definition) (agent.Message, error) {
				calls++
				if calls == 1 {
					m := healthCall()
					switch scenario {
					case "text_only":
						m.Content = []agent.Block{{Type: "text", Text: "OK"}}
					case "wrong_tool":
						m.Content[0].Name = "bash"
					case "invalid_arguments":
						m.Content[0].Input = json.RawMessage(`{"ready":false}`)
					case "multiple_calls":
						m.Content = append(m.Content, m.Content[0])
					case "truncated_first":
						m.StopReason = "max_tokens"
					}
					return m, nil
				}
				m := healthReceipt(t, messages)
				switch scenario {
				case "wrong_receipt":
					m.Content[0].Text = `{"ready":"invented"}`
				case "duplicate_receipt":
					m.Content[0].Text = `{"ready":"invented",` + m.Content[0].Text[1:]
				case "extra_call":
					m.Content = append(m.Content, healthCall().Content[0])
				case "truncated_last":
					m.StopReason = "length"
				}
				return m, nil
			})
			err := probeHealth(context.Background(), p)
			var failure *healthFailure
			want := "unverified"
			if !errors.As(err, &failure) || failure.Kind != want || calls > 2 {
				t.Fatalf("invalid probe outcome: calls=%d err=%v", calls, err)
			}
		})
	}
}

func TestHealthPreservesFailureClassificationAndCancellation(t *testing.T) {
	for _, tc := range []struct {
		kind agent.ErrorKind
		want string
	}{
		{agent.ErrorTransport, "transient"}, {agent.ErrorRateLimit, "transient"}, {agent.ErrorUnavailable, "transient"},
		{agent.ErrorProvider, "unverified"}, {agent.ErrorContextOverflow, "unverified"},
	} {
		t.Run(string(tc.kind), func(t *testing.T) {
			cause := &agent.ModelError{Kind: tc.kind, Err: errors.New("fixture")}
			err := probeHealth(context.Background(), healthProvider(func(context.Context, []agent.Message, []agent.Definition) (agent.Message, error) {
				return agent.Message{}, cause
			}))
			var failure *healthFailure
			if !errors.As(err, &failure) || failure.Kind != tc.want || !errors.Is(err, cause) {
				t.Fatalf("lost provider cause: %v", err)
			}
		})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	calls := 0
	err := probeHealth(ctx, healthProvider(func(ctx context.Context, _ []agent.Message, _ []agent.Definition) (agent.Message, error) {
		calls++
		if calls == 1 {
			return healthCall(), nil
		}
		<-ctx.Done()
		return agent.Message{}, ctx.Err()
	}))
	var failure *healthFailure
	if !errors.As(err, &failure) || failure.Kind != "unverified" || !errors.Is(err, context.DeadlineExceeded) || calls != 2 {
		t.Fatalf("probe did not share its deadline: calls=%d err=%v", calls, err)
	}
	unauthorized := &agent.ModelError{Kind: agent.ErrorProvider, Err: &provider.HTTPError{Status: 401}}
	if !errors.As(healthRequestFailure(unauthorized), &failure) || failure.Kind != "configuration" {
		t.Fatal("authentication failure was classified as model incompatibility")
	}
	for _, status := range []int{400, 404, 405, 415, 422} {
		cause := &agent.ModelError{Kind: agent.ErrorProvider, Err: &provider.HTTPError{Status: status}}
		if !errors.As(healthRequestFailure(cause), &failure) || failure.Kind != "incompatible" {
			t.Fatalf("explicit HTTP protocol rejection was not retained: %d", status)
		}
	}
	overflow := &agent.ModelError{Kind: agent.ErrorContextOverflow, Err: &provider.HTTPError{Status: 400}}
	if !errors.As(healthRequestFailure(overflow), &failure) || failure.Kind != "unverified" {
		t.Fatal("a bounded probe's overflow was treated as backend incompatibility")
	}
	if !errors.As(healthRequestFailure(errors.New("unknown response failure")), &failure) || failure.Kind != "unverified" {
		t.Fatal("an unknown failure claimed backend incompatibility")
	}
}
