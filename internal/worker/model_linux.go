//go:build linux

package worker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
	"pwnmesh/internal/agent"
	"pwnmesh/internal/modelrpc"
	"pwnmesh/internal/provider"
)

type bridgeModel struct {
	sizer     *provider.Anthropic
	runDir    string
	output    io.Writer
	sessionID string
	timeout   time.Duration
}

func modelForJob(j Job, o Options, sessionID string) (*bridgeModel, error) {
	if o.RunDir == "" || o.Output == nil {
		return nil, errors.New("worker requires the dispatcher model bridge")
	}
	raw, err := readModelFile(filepath.Join(o.RunDir, "model-settings.json"), 64<<10)
	if err != nil {
		return nil, fmt.Errorf("read dispatcher model settings: %w", err)
	}
	var settings modelrpc.Settings
	if err = json.Unmarshal(raw, &settings); err != nil {
		return nil, errors.New("invalid dispatcher model settings")
	}
	if settings.MaxTokens <= 0 || settings.Timeout <= 0 ||
		(settings.ReasoningEffort != "low" && settings.ReasoningEffort != "high" && settings.ReasoningEffort != "max") ||
		(j.Budget.ReasoningEffort != "" && j.Budget.ReasoningEffort != settings.ReasoningEffort) {
		return nil, errors.New("invalid dispatcher model policy")
	}
	if sessionID == "" {
		sessionID = j.RunID
	}
	// This credential-free value is used only for exact request sizing. Model
	// generation always crosses the dispatcher bridge, including child Agents.
	sizer := &provider.Anthropic{Model: settings.Model, MaxTokens: settings.MaxTokens, ReasoningEffort: settings.ReasoningEffort}
	if settings.Adaptive {
		sizer.BaseURL = "https://openrouter.ai"
	}
	return &bridgeModel{sizer: sizer, runDir: o.RunDir, output: o.Output, sessionID: sessionID, timeout: settings.Timeout}, nil
}

func (p *bridgeModel) InputBytes(messages []agent.Message, tools []agent.Definition) (int, error) {
	return p.sizer.InputBytes(messages, tools)
}

func (p *bridgeModel) Generate(ctx context.Context, messages []agent.Message, tools []agent.Definition, emit agent.Emit) (agent.Message, error) {
	return p.generate(ctx, messages, tools, 0, emit)
}

func (p *bridgeModel) GenerateSummary(ctx context.Context, messages []agent.Message, tokens int, emit agent.Emit) (agent.Message, error) {
	if tokens <= 0 {
		return agent.Message{}, errors.New("summary output allowance must be positive")
	}
	return p.generate(ctx, messages, nil, min(tokens, p.sizer.MaxTokens), emit)
}

func (p *bridgeModel) generate(parent context.Context, messages []agent.Message, tools []agent.Definition, tokens int, emit agent.Emit) (agent.Message, error) {
	if err := parent.Err(); err != nil {
		return agent.Message{}, err
	}
	// The provider owns its request timeout and receives the original phase
	// deadline. After either expires, allow only its final diagnostic to cross
	// the bridge; this allowance never extends model or tool execution.
	wait := p.timeout
	if wait <= time.Duration(1<<63-1)-modelrpc.DeliveryTimeout {
		wait += modelrpc.DeliveryTimeout
	}
	waitDeadline := time.Now().Add(wait)
	deadline, hasDeadline := parent.Deadline()
	if hasDeadline && deadline.Add(modelrpc.DeliveryTimeout).Before(waitDeadline) {
		waitDeadline = deadline.Add(modelrpc.DeliveryTimeout)
	}
	ctx, cancel := context.WithDeadline(context.WithoutCancel(parent), waitDeadline)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return agent.Message{}, err
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return agent.Message{}, err
	}
	id := hex.EncodeToString(nonce[:])
	request := modelrpc.Request{RequestID: id, SessionID: p.sessionID, Messages: messages, Tools: tools, SummaryTokens: tokens, Deadline: deadline}
	raw, err := json.Marshal(modelrpc.RequestEvent{Type: "model_request", Request: request})
	if err != nil {
		return agent.Message{}, err
	}
	if len(raw)+1 > modelrpc.MaxRequestBytes {
		return agent.Message{}, &agent.ModelError{Kind: agent.ErrorBudget, Err: errors.New("model bridge request exceeds 32 MiB")}
	}
	if err = parent.Err(); err != nil {
		return agent.Message{}, err
	}
	if err = writeModelFrame(p.output, raw); err != nil {
		return agent.Message{}, &agent.ModelError{Kind: agent.ErrorTransport, Err: err}
	}
	name := filepath.Join(p.runDir, "model-response-"+id+".json")
	defer os.Remove(name)
	stop := func() {
		raw, _ := json.Marshal(modelrpc.CancelEvent{Type: "model_cancel", RequestID: id})
		_ = writeModelFrame(p.output, raw)
	}
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	parentDone := parent.Done()
	for {
		if parent.Err() == context.Canceled {
			stop()
			return agent.Message{}, context.Canceled
		}
		if parent.Err() == context.DeadlineExceeded {
			// The dispatcher already has this deadline. Sending model_cancel
			// here could race its timer and erase the real deadline diagnosis.
			parentDone = nil
		}
		if err = ctx.Err(); err != nil {
			stop()
			if parent.Err() != nil {
				err = parent.Err()
			}
			return agent.Message{}, err
		}
		var response modelrpc.Response
		raw, err = readModelFile(name, modelrpc.MaxResponseBytes)
		if err == nil {
			_ = os.Remove(name)
			if json.Unmarshal(raw, &response) != nil || response.RequestID != id {
				return agent.Message{}, errors.New("invalid dispatcher model response")
			}
			if parentErr := parent.Err(); parentErr != nil {
				// A late success cannot restart the loop. Retain only an actual
				// classified failure from this request for recovery policy.
				if parentErr == context.DeadlineExceeded && response.Error != nil && (response.Error.Kind != "" || response.Error.HTTPStatus != 0) {
					return agent.Message{}, errors.Join(parentErr, response.Error.Err())
				}
				return agent.Message{}, parentErr
			}
			if emit != nil {
				for _, event := range response.Events {
					emit(event)
				}
			}
			return response.Message, response.Error.Err()
		}
		if !os.IsNotExist(err) {
			return agent.Message{}, err
		}
		select {
		case <-parentDone:
		case <-ctx.Done():
		case <-tick.C:
		}
	}
}

func writeModelFrame(output io.Writer, raw []byte) error {
	frame := append(raw, '\n')
	n, err := output.Write(frame)
	if err == nil && n != len(frame) {
		err = io.ErrShortWrite
	}
	return err
}

func readModelFile(name string, limit int64) ([]byte, error) {
	fd, err := unix.Open(name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !stat.Mode().IsRegular() || stat.Size() > limit {
		return nil, errors.New("invalid dispatcher model file")
	}
	raw, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err == nil && int64(len(raw)) > limit {
		err = errors.New("dispatcher model file exceeds its size limit")
	}
	return raw, err
}
