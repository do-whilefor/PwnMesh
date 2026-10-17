package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"path"
	"strings"
	"sync"

	"pwnmesh/internal/agent"
	"pwnmesh/internal/config"
	"pwnmesh/internal/modelrpc"
	"pwnmesh/internal/protocol"
	"pwnmesh/internal/provider"
)

// Each bridge is bound to one launch. Only the dispatcher sees the upstream
// endpoint and credentials; worker messages cannot choose either of them.
const maxModelConcurrency = 16

type modelBridge struct {
	ctx       context.Context
	cancel    context.CancelFunc
	job       protocol.Job
	model     *provider.Client
	deliver   func(context.Context, modelrpc.Response) error
	fail      func(error)
	mu        sync.Mutex
	active    map[string]context.CancelFunc
	seen      map[string]bool
	wg        sync.WaitGroup
	slots     chan struct{}
	admission chan struct{}
}

func configuredModel(w config.Worker, j protocol.Job) (*provider.Client, error) {
	return provider.FromEnvironment(w.Getenv, j.Budget.ReasoningEffort)
}

func publicModelSettings(p *provider.Client) modelrpc.Settings {
	model := p.Model
	if model == "" {
		model = provider.DefaultModel
	}
	return modelrpc.Settings{Protocol: p.Protocol, Model: model, MaxTokens: p.MaxTokens, ReasoningEffort: p.ReasoningEffort, Timeout: p.Timeout, Adaptive: p.AdaptiveThinking()}
}

func modelWorkerEnv(w config.Worker, secrets ...string) ([]string, error) {
	values, err := w.ToolEnvironment()
	if err != nil {
		return nil, err
	}
	env := []string{"PWNMESH_MODEL_BRIDGE=dispatcher-v1"}
	for key, value := range values {
		if containsModelCredential(key+"="+value, secrets...) {
			return nil, errors.New("model credential is duplicated in the tool environment")
		}
		env = append(env, key+"="+value)
	}
	return env, nil
}

func containsModelCredential(value string, secrets ...string) bool {
	for _, secret := range secrets {
		if secret != "" && strings.Contains(value, secret) {
			return true
		}
	}
	return false
}

// Inspect decoded strings at every JSON depth, including object keys and raw
// tool arguments. Token scanning also checks duplicate keys that unmarshaling
// into a map would discard. Invalid JSON fails closed.
func jsonContainsModelCredential(raw []byte, secrets ...string) bool {
	if !json.Valid(raw) {
		return true
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			return false
		}
		if err != nil {
			return true
		}
		if value, ok := token.(string); ok && containsModelCredential(value, secrets...) {
			return true
		}
	}
}

func newModelBridge(parent context.Context, p *provider.Client, j protocol.Job, deliver func(context.Context, modelrpc.Response) error, fail func(error)) *modelBridge {
	ctx, cancel := context.WithCancel(parent)
	return &modelBridge{ctx: ctx, cancel: cancel, model: p, job: j, deliver: deliver, fail: fail, active: map[string]context.CancelFunc{}, seen: map[string]bool{}, slots: make(chan struct{}, 32)}
}

func (b *modelBridge) close() {
	// Pair cancellation with start's context check and WaitGroup.Add. Once the
	// lock is released no new call can be admitted while Wait observes zero.
	b.mu.Lock()
	b.cancel()
	b.mu.Unlock()
	b.wg.Wait()
}

func (b *modelBridge) stop(id string) error {
	if !protocol.ValidGraphRequestID(id) {
		return errors.New("invalid model cancellation ID")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if cancel := b.active[id]; cancel != nil {
		cancel()
	}
	return nil
}

func (b *modelBridge) start(r modelrpc.Request) error {
	if !protocol.ValidGraphRequestID(r.RequestID) {
		return errors.New("invalid model request ID")
	}
	if r.SessionID != b.job.RunID {
		parts := strings.Split(r.SessionID, ":")
		if len(parts) != 3 || parts[0] != b.job.RunID || b.job.Kind != "explore" || !modelSessionPart(parts[1]) || !modelSessionPart(parts[2]) {
			return errors.New("model session does not belong to this run")
		}
	}
	if r.SummaryTokens < 0 || r.SummaryTokens > b.model.MaxTokens {
		return errors.New("invalid model summary allowance")
	}
	if r.SummaryTokens > 0 && len(r.Tools) > 0 {
		return errors.New("summary requests cannot declare tools")
	}
	raw, err := json.Marshal(r)
	if err != nil || len(raw) > modelrpc.MaxRequestBytes {
		return errors.New("model request exceeds its byte limit")
	}
	// A published response can trigger another turn before its Docker exec has
	// finished. Bound those overlapping deliveries with cancelable backpressure,
	// without holding the lock that delivery completion and close require.
	select {
	case b.slots <- struct{}{}:
	case <-b.ctx.Done():
		return b.ctx.Err()
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.ctx.Err() != nil {
		<-b.slots
		return b.ctx.Err()
	}
	if b.seen[r.RequestID] || len(b.seen) >= 4096 || len(b.active) >= maxModelConcurrency {
		<-b.slots
		return errors.New("model bridge request limit or duplicate ID")
	}
	ctx, cancel := context.WithTimeout(b.ctx, b.model.Timeout)
	if !r.Deadline.IsZero() {
		var shorten context.CancelFunc
		ctx, shorten = context.WithDeadline(ctx, r.Deadline)
		original := cancel
		cancel = func() { shorten(); original() }
	}
	b.seen[r.RequestID], b.active[r.RequestID] = true, cancel
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		defer cancel()
		defer func() { <-b.slots }()
		response := b.generateAdmitted(ctx, r)
		// Free the model slot before publishing its response. The worker can
		// issue its next turn as soon as mv publishes, before Docker exec exits.
		b.mu.Lock()
		delete(b.active, r.RequestID)
		b.mu.Unlock()
		// A canceled individual call still needs a typed reply; the enclosing
		// launch owns delivery. Finishing a launch cancels every outstanding call.
		if b.ctx.Err() == nil {
			if err := b.deliver(b.ctx, response); err != nil {
				b.fail(errors.New("model bridge response delivery failed"))
			}
		}
	}()
	return nil
}

// Wait inside the request goroutine so model cancellation and graph frames can
// still be consumed. Delivery has its own bound and never holds provider capacity.
func (b *modelBridge) generateAdmitted(ctx context.Context, r modelrpc.Request) modelrpc.Response {
	if b.admission != nil {
		select {
		case b.admission <- struct{}{}:
			defer func() { <-b.admission }()
		case <-ctx.Done():
			return modelrpc.Response{RequestID: r.RequestID, Error: safeModelError(ctx.Err())}
		}
	}
	if err := ctx.Err(); err != nil {
		return modelrpc.Response{RequestID: r.RequestID, Error: safeModelError(err)}
	}
	return b.generate(ctx, r)
}

func modelSessionPart(s string) bool {
	if len(s) == 0 || len(s) > 128 {
		return false
	}
	for _, ch := range s {
		if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '-' || ch == '_') {
			return false
		}
	}
	return true
}

func safeModelError(err error) *modelrpc.Error {
	if err == nil {
		return nil
	}
	out := &modelrpc.Error{Kind: agent.ErrorProvider, Canceled: errors.Is(err, context.Canceled), Deadline: errors.Is(err, context.DeadlineExceeded)}
	var model *agent.ModelError
	if errors.As(err, &model) {
		switch model.Kind {
		case agent.ErrorContextOverflow, agent.ErrorTransport, agent.ErrorRateLimit, agent.ErrorUnavailable, agent.ErrorProvider, agent.ErrorToolArguments, agent.ErrorBudget:
			out.Kind = model.Kind
		}
	} else if out.Canceled || out.Deadline {
		out.Kind = ""
	}
	var endpoint *provider.HTTPError
	if errors.As(err, &endpoint) && endpoint.Status >= 100 && endpoint.Status <= 599 {
		out.HTTPStatus = endpoint.Status
	}
	return out
}

func (b *modelBridge) generate(ctx context.Context, r modelrpc.Request) modelrpc.Response {
	// A fresh provider keeps simultaneous child sessions independent. Their
	// identity comes from this run, and all upstream policy remains fixed.
	p := &provider.Client{AuthMode: b.model.AuthMode, TokenSource: b.model.TokenSource, Secrets: b.model.Secrets, Protocol: b.model.Protocol, BaseURL: b.model.BaseURL, Token: b.model.Token, Model: b.model.Model, MaxTokens: b.model.MaxTokens, ReasoningEffort: b.model.ReasoningEffort, Timeout: b.model.Timeout, Client: b.model.Client, SessionID: r.SessionID}
	out := modelrpc.Response{RequestID: r.RequestID}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var eventBytes int
	tooLarge := false
	emit := func(e agent.Event) {
		raw, err := json.Marshal(e)
		eventBytes += len(raw)
		if err != nil || eventBytes > 32<<20 {
			tooLarge = true
			cancel()
			return
		}
		out.Events = append(out.Events, e)
	}
	var err error
	if r.SummaryTokens > 0 {
		out.Message, err = p.GenerateSummary(ctx, r.Messages, r.SummaryTokens, emit)
	} else {
		out.Message, err = p.Generate(ctx, r.Messages, r.Tools, emit)
	}
	out.Error = safeModelError(err)
	raw, marshalErr := json.Marshal(out)
	// Delay delta publication until the complete response can be checked. A
	// malicious upstream could echo an authentication token split across deltas.
	var text strings.Builder
	for _, e := range out.Events {
		text.WriteString(e.Text)
	}
	if tooLarge || marshalErr != nil || len(raw) > modelrpc.MaxResponseBytes || jsonContainsModelCredential(raw, p.CredentialValues()...) || containsModelCredential(text.String(), p.CredentialValues()...) {
		// Numeric usage is independent of rejected content and remains an observation
		// even when the response cannot cross the credential/size boundary.
		return modelrpc.Response{RequestID: r.RequestID, Message: agent.Message{Usage: out.Message.Usage}, Error: &modelrpc.Error{Kind: agent.ErrorProvider}}
	}
	return out
}

func (c *Client) modelDelivery(name, runDir string) func(context.Context, modelrpc.Response) error {
	return func(parent context.Context, response modelrpc.Response) error {
		ctx, cancel := context.WithTimeout(parent, modelrpc.DeliveryTimeout)
		defer cancel()
		raw, err := json.Marshal(response)
		if err != nil {
			return err
		}
		target := path.Join(runDir, "model-response-"+response.RequestID+".json")
		return c.publish(ctx, name, target, raw)
	}
}
