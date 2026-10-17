package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"pwnmesh/internal/agent"
	"pwnmesh/internal/config"
	"pwnmesh/internal/modelrpc"
	"pwnmesh/internal/protocol"
	"pwnmesh/internal/provider"
)

func TestModelBridgeBindsPolicyAndKeepsCredentialsOutOfWorker(t *testing.T) {
	const secret = "fixture-private-model-key"
	backend := config.Worker{Env: map[string]string{
		"ANTHROPIC_AUTH_TOKEN": secret, "ANTHROPIC_MODEL": "fixture-model",
		"ANTHROPIC_BASE_URL":       "https://openrouter.ai/api/v1?key=private-endpoint",
		"PWNMESH_REASONING_EFFORT": "low", "PWNMESH_MAX_OUTPUT_TOKENS": "2048", "PWNMESH_REQUEST_TIMEOUT": "45", "TOOL_SETTING": "kept",
	}}
	j := protocol.Job{RunID: "run", Kind: "explore", Budget: config.Task{ReasoningEffort: "high"}}
	p, err := configuredModel(backend, j)
	if err != nil {
		t.Fatal(err)
	}
	settings := publicModelSettings(p)
	raw, _ := json.Marshal(settings)
	if strings.Contains(string(raw), secret) || strings.Contains(string(raw), "private-endpoint") || !settings.Adaptive || settings.Model != "fixture-model" || settings.ReasoningEffort != "high" || settings.Timeout != 45*time.Second {
		t.Fatal("invalid public model settings")
	}
	env, err := modelWorkerEnv(backend, secret)
	if err != nil || strings.Contains(strings.Join(env, "\n"), "ANTHROPIC_") || !strings.Contains(strings.Join(env, "\n"), "TOOL_SETTING=kept") {
		t.Fatal("unsafe or incomplete tool environment")
	}
	backend.Env["ALIASED_KEY"] = secret
	if _, err := modelWorkerEnv(backend, secret); err == nil {
		t.Fatal("duplicated provider credential passed to tools")
	}

	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Model        string          `json:"model"`
			MaxTokens    int             `json:"max_tokens"`
			Messages     []agent.Message `json:"messages"`
			OutputConfig struct {
				Effort string `json:"effort"`
			} `json:"output_config"`
		}
		if r.Header.Get("x-api-key") != secret || r.Header.Get("Authorization") != "Bearer "+secret || r.Header.Get("x-opencode-session") != "run:graph:child" {
			t.Error("model call lost its dispatcher authentication or session")
		}
		if json.NewDecoder(r.Body).Decode(&input) != nil || input.Model != "fixture-model" || input.MaxTokens != 512 || input.OutputConfig.Effort != "high" || len(input.Messages[0].Text()) < 128<<10 {
			t.Error("model policy or large request changed across bridge")
		}
		io.WriteString(w, `{"role":"assistant","content":[{"type":"text","text":"summary"}],"stop_reason":"end_turn","usage":{"input_tokens":12,"output_tokens":3}}`)
	}))
	defer model.Close()
	p.BaseURL = model.URL
	b := newModelBridge(context.Background(), p, j, nil, nil)
	defer b.close()
	out := b.generate(context.Background(), modelrpc.Request{RequestID: strings.Repeat("a", 32), SessionID: "run:graph:child", SummaryTokens: 512, Messages: []agent.Message{agent.Text("user", strings.Repeat("x", 200<<10))}})
	if out.Error != nil || out.Message.Text() != "summary" || out.Message.Usage == nil || out.Message.Usage.OutputTokens != 3 {
		t.Fatalf("model response lost: %+v", out)
	}
}

func TestModelBridgeRetainsOpenAIProtocolForGenerationAndSummary(t *testing.T) {
	const secret = "openai-private-fixture"
	for _, summaryTokens := range []int{0, 512} {
		model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var input struct {
				Model     string `json:"model"`
				MaxTokens int    `json:"max_completion_tokens"`
			}
			limit := 2048
			if summaryTokens > 0 {
				limit = summaryTokens
			}
			if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer "+secret || r.Header.Get("x-api-key") != "" || json.NewDecoder(r.Body).Decode(&input) != nil || input.Model != "openai-model" || input.MaxTokens != limit {
				t.Error("bridge lost OpenAI protocol or output budget")
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"complete"}}],"usage":{"prompt_tokens":12,"completion_tokens":3}}`)
		}))
		backend := config.Worker{Env: map[string]string{"PWNMESH_MODEL_PROTOCOL": "openai", "OPENAI_API_KEY": secret, "OPENAI_BASE_URL": model.URL, "OPENAI_MODEL": "openai-model", "PWNMESH_MAX_OUTPUT_TOKENS": "2048"}}
		p, err := configuredModel(backend, protocol.Job{})
		if err != nil {
			t.Fatal(err)
		}
		settings := publicModelSettings(p)
		raw, _ := json.Marshal(settings)
		if settings.Protocol != "openai" || settings.Adaptive || strings.Contains(string(raw), secret) || strings.Contains(string(raw), model.URL) {
			t.Fatal("public settings lost protocol or exposed endpoint credentials")
		}
		bridge := newModelBridge(context.Background(), p, protocol.Job{}, nil, nil)
		out := bridge.generate(context.Background(), modelrpc.Request{RequestID: strings.Repeat("a", 32), SessionID: "run", SummaryTokens: summaryTokens, Messages: []agent.Message{agent.Text("user", "finish")}})
		bridge.close()
		model.Close()
		if out.Error != nil || out.Message.Text() != "complete" || out.Message.Usage == nil || out.Message.Usage.Input() != 12 {
			t.Fatalf("OpenAI response failed across bridge: %+v", out)
		}
	}
}

func TestModelBridgePreservesOpenAIArgumentRepairError(t *testing.T) {
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call-1\",\"type\":\"function\",\"function\":{\"name\":\"inspect\",\"arguments\":\"{\\\"path\\\":\"}}]},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":12}}\n\ndata: [DONE]\n\n")
	}))
	defer model.Close()
	p := &provider.Client{Protocol: "openai", BaseURL: model.URL, Token: "fixture-secret", Model: "model", MaxTokens: 1024, Timeout: time.Second}
	bridge := newModelBridge(context.Background(), p, protocol.Job{}, nil, nil)
	defer bridge.close()
	out := bridge.generate(context.Background(), modelrpc.Request{RequestID: strings.Repeat("a", 32), Messages: []agent.Message{agent.Text("user", "inspect")}})
	if out.Error == nil || out.Error.Kind != agent.ErrorToolArguments || out.Message.Usage == nil || out.Message.Usage.Input() != 12 {
		t.Fatalf("OpenAI argument repair classification lost across bridge: %+v", out)
	}
}

func TestModelBridgePreservesTypedErrorsWithoutDetails(t *testing.T) {
	for _, kind := range []agent.ErrorKind{agent.ErrorTransport, agent.ErrorRateLimit, agent.ErrorUnavailable, agent.ErrorContextOverflow, agent.ErrorToolArguments, agent.ErrorProvider} {
		err := errors.Join(&agent.ModelError{Kind: kind, Err: &provider.HTTPError{Status: 429}}, context.DeadlineExceeded, errors.New("https://private.invalid?token=fixture-secret"))
		wire := safeModelError(err)
		raw, _ := json.Marshal(wire)
		var restored *agent.ModelError
		var endpoint *provider.HTTPError
		if strings.Contains(string(raw), "fixture-secret") || !errors.As(wire.Err(), &restored) || restored.Kind != kind || !errors.As(wire.Err(), &endpoint) || endpoint.Status != 429 || !errors.Is(wire.Err(), context.DeadlineExceeded) {
			t.Fatalf("error classification lost for %s", kind)
		}
	}
	if !errors.Is(safeModelError(context.Canceled).Err(), context.Canceled) {
		t.Fatal("cancellation became a permanent model failure")
	}
}

func TestModelBridgeRejectsSecretEchoIncludingSplitDeltas(t *testing.T) {
	const secret = "model-secret-never-in-workspace"
	for _, mode := range []string{"json", "tool", "split_stream", "error"} {
		t.Run(mode, func(t *testing.T) {
			model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch mode {
				case "error":
					w.WriteHeader(http.StatusUnauthorized)
					fmt.Fprintf(w, `{"error":{"message":%q}}`, secret)
				case "split_stream":
					w.Header().Set("Content-Type", "text/event-stream")
					io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"role\":\"assistant\",\"content\":[]}}\n\n")
					io.WriteString(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
					for _, text := range []string{secret[:10], secret[10:]} {
						fmt.Fprintf(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":%q}}\n\n", text)
					}
					io.WriteString(w, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
				case "tool":
					fmt.Fprintf(w, `{"role":"assistant","content":[{"type":"tool_use","id":"call","name":"bash","input":{"command":%q}}],"stop_reason":"tool_use"}`, secret)
				default:
					fmt.Fprintf(w, `{"role":"assistant","content":[{"type":"text","text":%q}],"stop_reason":"end_turn"}`, secret)
				}
			}))
			defer model.Close()
			b := newModelBridge(context.Background(), &provider.Client{BaseURL: model.URL, Token: secret}, protocol.Job{}, nil, nil)
			defer b.close()
			out := b.generate(context.Background(), modelrpc.Request{RequestID: strings.Repeat("b", 32), SessionID: "run"})
			raw, _ := json.Marshal(out)
			if out.Error == nil || strings.Contains(string(raw), secret) || len(out.Events) != 0 || len(out.Message.Content) != 0 {
				t.Fatalf("secret echo escaped dispatcher in %s", mode)
			}
		})
	}
}

func TestModelBridgeConcurrentCancellationAndRunBinding(t *testing.T) {
	arrived := make(chan string, 16)
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		arrived <- r.Header.Get("x-opencode-session")
		<-r.Context().Done()
	}))
	defer model.Close()
	p := &provider.Client{BaseURL: model.URL, Token: "safe-private-key", MaxTokens: 1024, Timeout: time.Minute}
	responses := make(chan modelrpc.Response, 16)
	b := newModelBridge(context.Background(), p, protocol.Job{RunID: "run", Kind: "explore"}, func(_ context.Context, response modelrpc.Response) error { responses <- response; return nil }, func(error) { t.Error("unexpected delivery error") })
	defer b.close()
	requests := []modelrpc.Request{{RequestID: strings.Repeat("a", 32), SessionID: "run:g:left"}, {RequestID: strings.Repeat("b", 32), SessionID: "run:g:right"}}
	for _, request := range requests {
		if err := b.start(request); err != nil {
			t.Fatal(err)
		}
	}
	for range requests {
		select {
		case <-arrived:
		case <-time.After(5 * time.Second):
			t.Fatal("parallel model request was serialized")
		}
	}
	for _, request := range requests {
		if err := b.start(request); err == nil {
			t.Fatal("duplicate model request accepted")
		}
		if err := b.stop(request.RequestID); err != nil {
			t.Fatal(err)
		}
	}
	for range requests {
		select {
		case out := <-responses:
			if out.Error == nil || !errors.Is(out.Error.Err(), context.Canceled) {
				t.Fatal("cancellation classification lost")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("model cancellation did not settle")
		}
	}
	for _, request := range []modelrpc.Request{
		{RequestID: "../escape", SessionID: "run"},
		{RequestID: strings.Repeat("c", 32), SessionID: "other-run"},
		{RequestID: strings.Repeat("d", 32), SessionID: "run", SummaryTokens: 1025},
		{RequestID: strings.Repeat("e", 32), SessionID: "run", SummaryTokens: 1, Tools: []agent.Definition{{Name: "bash"}}},
	} {
		if err := b.start(request); err == nil {
			t.Fatal("invalid model request accepted")
		}
	}
	b.close()
	if err := b.start(modelrpc.Request{RequestID: strings.Repeat("f", 32), SessionID: "run"}); err == nil {
		t.Fatal("closed run started a model request")
	}
}

func TestResultSinkRoutesModelFramesWithoutBlockingGraph(t *testing.T) {
	var order []string
	sink := resultSink{
		model:       func(modelrpc.Request) error { order = append(order, "model"); return nil },
		cancelModel: func(string) error { order = append(order, "cancel"); return nil },
		graph:       func(protocol.GraphRequest) error { order = append(order, "graph"); return nil },
	}
	input := "{\"type\":\"model_request\",\"request\":{}}\n{\"type\":\"graph_request\",\"request\":{}}\n{\"type\":\"model_cancel\",\"request_id\":\"a\"}\n"
	for _, ch := range []byte(input) {
		if _, err := sink.Write([]byte{ch}); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Join(order, ",") != "model,graph,cancel" {
		t.Fatal(order)
	}
}

func TestModelBridgeSharesAdmissionWithoutBlockingCancellationOrDelivery(t *testing.T) {
	arrived := make(chan string, 4)
	release := make(chan struct{}, 4)
	client := &http.Client{Transport: inputTestTransport(func(r *http.Request) (*http.Response, error) {
		arrived <- r.Header.Get("x-opencode-session")
		select {
		case <-release:
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"role":"assistant","content":[{"type":"text","text":"fixture"}],"stop_reason":"end_turn"}`))}, nil
	})}
	pool := make(chan struct{}, 1)
	responses := make(chan modelrpc.Response, 4)
	deliveryRelease := make(chan struct{})
	makeBridge := func(run string) *modelBridge {
		b := newModelBridge(context.Background(), &provider.Client{Token: "fixture", Model: "fixture", MaxTokens: 32, Timeout: 5 * time.Second, Client: client}, protocol.Job{RunID: run}, func(ctx context.Context, r modelrpc.Response) error {
			responses <- r
			select {
			case <-deliveryRelease:
			case <-ctx.Done():
			}
			return nil
		}, func(err error) { t.Errorf("delivery: %v", err) })
		b.admission = pool
		t.Cleanup(b.close)
		return b
	}
	first, second := makeBridge("first"), makeBridge("second")
	request := func(id int, run string) modelrpc.Request {
		return modelrpc.Request{RequestID: fmt.Sprintf("%032x", id), SessionID: run}
	}
	start := func(b *modelBridge, r modelrpc.Request) {
		t.Helper()
		if err := b.start(r); err != nil {
			t.Fatal(err)
		}
	}
	waitArrival := func(want string) {
		t.Helper()
		select {
		case got := <-arrived:
			if got != want {
				t.Fatalf("provider saw %q, want %q", got, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("provider admission did not advance")
		}
	}
	start(first, request(1, "first"))
	waitArrival("first")
	start(second, request(2, "second"))
	select {
	case got := <-arrived:
		t.Fatalf("shared limit admitted %q while occupied", got)
	case <-time.After(30 * time.Millisecond):
	}
	if err := second.stop(request(2, "second").RequestID); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-responses:
		if r.RequestID != request(2, "second").RequestID || r.Error == nil || !errors.Is(r.Error.Err(), context.Canceled) {
			t.Fatalf("queued cancellation lost its typed response: %+v", r)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("queued cancellation waited for provider capacity")
	}
	start(second, request(3, "second"))
	release <- struct{}{}
	waitArrival("second") // First delivery is still blocked, but capacity was freed.
	release <- struct{}{}
	close(deliveryRelease)
	first.close()
	second.close()
	if len(pool) != 0 {
		t.Fatal("closed bridges leaked shared admission")
	}
	select {
	case got := <-arrived:
		t.Fatalf("cancelled queued request reached provider: %s", got)
	default:
	}
}

func TestClientModelAdmissionIsSharedAndBounded(t *testing.T) {
	c := &Client{}
	if first, second := c.modelAdmission(), c.modelAdmission(); first != second || cap(first) != maxModelConcurrency {
		t.Fatal("Worker launches do not share the dispatcher model limit")
	}
}
