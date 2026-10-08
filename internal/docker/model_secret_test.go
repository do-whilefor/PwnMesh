package docker

import (
	"archive/tar"
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
	"pwnmesh/internal/board"
	"pwnmesh/internal/config"
	"pwnmesh/internal/modelrpc"
	"pwnmesh/internal/provider"
	"pwnmesh/internal/worker"
)

func TestModelCredentialJSONChecksDecodedStringsAndDuplicateKeys(t *testing.T) {
	const secret = "fixture<\"\\key"
	escaped := unicodeSecret(secret)
	for _, raw := range []string{
		`{"value":"prefix` + escaped + `suffix"}`,
		`{"outer":[{"inner":{"value":"` + escaped + `"}}]}`,
		`{"` + escaped + `":null}`,
		`{"duplicate":"` + escaped + `","duplicate":"clean"}`,
	} {
		wrapped, err := json.Marshal(struct {
			Input json.RawMessage `json:"input"`
		}{json.RawMessage(raw)})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(wrapped), secret) {
			t.Fatal("fixture did not exercise JSON escaping")
		}
		if !jsonContainsModelCredential(wrapped, "", secret) {
			t.Error("escaped credential passed the JSON boundary")
		}
	}
	settings, err := json.Marshal(modelrpc.Settings{Model: "model-" + secret})
	if err != nil || !jsonContainsModelCredential(settings, secret) {
		t.Fatal("public model settings did not receive decoded-string checking")
	}
	if jsonContainsModelCredential([]byte(`{"value":[null,true,1e999999,"clean"]}`), "", secret) {
		t.Fatal("clean JSON or an empty secret was treated as a credential")
	}
	for _, raw := range []string{`{"unfinished":`, `{} {}`, ""} {
		if !jsonContainsModelCredential([]byte(raw), secret) {
			t.Fatal("invalid JSON did not fail closed")
		}
	}
}

func TestModelWorkerEnvironmentRejectsLiteralCredentialAliases(t *testing.T) {
	const secret = "fixture<\"\\key"
	for _, env := range []map[string]string{
		{"MODEL_COPY": "prefix" + secret + "suffix"},
		{"ALIAS_" + secret: "value"},
	} {
		if _, err := modelWorkerEnv(config.Worker{Env: env}, secret); err == nil || strings.Contains(err.Error(), secret) {
			t.Fatalf("credential alias was accepted or exposed: %v", err)
		}
	}
	env, err := modelWorkerEnv(config.Worker{Env: map[string]string{"ANTHROPIC_AUTH_TOKEN": secret, "LANG": "C"}}, secret)
	if err != nil || containsModelCredential(strings.Join(env, "\n"), secret) {
		t.Fatalf("configured provider credential entered worker environment: %v", err)
	}
}

func TestModelBridgeRejectsEscapedCredentialEcho(t *testing.T) {
	const secret = "fixture<\"\\key"
	for _, block := range []agent.Block{
		{Type: "text", Text: "echo " + secret},
		{Type: "thinking", Thinking: secret},
		{Type: "tool_use", ID: "call", Name: "bash", Input: json.RawMessage(`{"command":"` + unicodeSecret(secret) + `"}`)},
	} {
		t.Run(block.Type, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(agent.Message{Role: "assistant", Content: []agent.Block{block}, StopReason: "end_turn"})
			}))
			defer upstream.Close()
			bridge := newModelBridge(context.Background(), &provider.Anthropic{Token: secret, BaseURL: upstream.URL, Model: "fixture", MaxTokens: 32, Timeout: time.Second}, worker.Job{RunID: "run"}, nil, nil)
			defer bridge.close()
			response := bridge.generate(context.Background(), modelrpc.Request{RequestID: strings.Repeat("a", 32), SessionID: "run", Messages: []agent.Message{agent.Text("user", "fixture")}})
			if response.Error == nil || len(response.Message.Content) != 0 || len(response.Events) != 0 {
				t.Fatal("upstream credential echo reached the worker response")
			}
		})
	}
}

func TestModelRunRejectsCredentialAliasesInImageAndContainer(t *testing.T) {
	const secret = "fixture<\"\\key"
	for _, source := range []string{"image", "container"} {
		t.Run(source, func(t *testing.T) {
			engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				alias := []string{"TOOL_PROXY=https://proxy.invalid/?auth=" + secret}
				switch {
				case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/containers/"):
					var env []string
					if source == "container" {
						env = alias
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"Image": "sha256:worker", "Config": map[string]any{"Env": env, "Labels": map[string]string{
						"pwnmesh.namespace": "test", "pwnmesh.project": "p", "pwnmesh.model-boundary": "dispatcher-v1",
					}}, "State": map[string]bool{"Running": true}, "HostConfig": map[string]string{"NetworkMode": "bridge"}})
				case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/images/"):
					_ = json.NewEncoder(w).Encode(map[string]any{"Id": "sha256:worker", "Config": map[string]any{"Env": alias}})
				default:
					t.Errorf("credential rejection must precede mutations: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusInternalServerError)
				}
			}))
			defer engine.Close()
			job := worker.Job{RunID: "run", Graph: board.Graph{Project: board.Project{ID: "p"}}}
			_, err := boundaryClient(engine.URL).Run(context.Background(), config.Worker{Env: map[string]string{"ANTHROPIC_AUTH_TOKEN": secret, "ANTHROPIC_MODEL": "fixture"}}, job)
			if err == nil || !strings.Contains(err.Error(), "model provider environment") || strings.Contains(err.Error(), secret) {
				t.Fatalf("credential alias was accepted or exposed: %v", err)
			}
		})
	}
}

func TestModelRunRejectsEscapedCredentialBeforePublishingSensitiveFiles(t *testing.T) {
	const secret = "fixture<\"\\key"
	for _, source := range []string{"job", "settings"} {
		t.Run(source, func(t *testing.T) {
			archives := 0
			engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/containers/"):
					io.WriteString(w, `{"Image":"sha256:worker","Config":{"Labels":{"pwnmesh.namespace":"test","pwnmesh.project":"p","pwnmesh.model-boundary":"dispatcher-v1"}},"State":{"Running":true},"HostConfig":{"NetworkMode":"bridge"}}`)
				case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/images/"):
					io.WriteString(w, `{"Id":"sha256:worker"}`)
				case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/archive"):
					archives++
					archive := tar.NewReader(r.Body)
					for {
						header, err := archive.Next()
						if err == io.EOF {
							break
						}
						if err != nil {
							t.Error(err)
							break
						}
						if header.Typeflag == tar.TypeReg {
							raw, err := io.ReadAll(archive)
							if err != nil || jsonContainsModelCredential(raw, secret) {
								t.Error("a credential reached the worker archive")
							}
						}
					}
				default:
					t.Errorf("unexpected engine request after unsafe configuration: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusInternalServerError)
				}
			}))
			defer engine.Close()
			job := worker.Job{RunID: "run", Graph: board.Graph{Project: board.Project{ID: "p"}}}
			backend := config.Worker{Env: map[string]string{"ANTHROPIC_AUTH_TOKEN": secret, "ANTHROPIC_MODEL": "fixture"}}
			wantError := "model credential must not be included in a worker job"
			if source == "job" {
				job.InputView = json.RawMessage(`{"nested":[{"value":"` + unicodeSecret(secret) + `"}]}`)
			} else {
				backend.Env["ANTHROPIC_MODEL"] = "model-" + secret
				wantError = "invalid public model settings"
			}
			_, err := boundaryClient(engine.URL).Run(context.Background(), backend, job)
			if err == nil || !strings.Contains(err.Error(), wantError) || strings.Contains(err.Error(), secret) {
				t.Fatalf("unsafe %s was accepted or exposed: %v", source, err)
			}
			if source == "job" && archives != 0 {
				t.Fatal("unsafe job was uploaded before being checked")
			}
		})
	}
}

func TestModelBridgeCloseRacingAdmissionJoinsAllCalls(t *testing.T) {
	client := &http.Client{Transport: inputTestTransport(func(r *http.Request) (*http.Response, error) {
		<-r.Context().Done()
		return nil, r.Context().Err()
	})}
	for i := 0; i < 256; i++ {
		bridge := newModelBridge(context.Background(), &provider.Anthropic{Token: "fixture-token", Model: "fixture", MaxTokens: 32, Timeout: time.Second, Client: client}, worker.Job{RunID: "run"}, func(context.Context, modelrpc.Response) error { return nil }, func(err error) { t.Error(err) })
		start := make(chan struct{})
		admitted := make(chan error, 1)
		closed := make(chan struct{})
		go func() {
			<-start
			admitted <- bridge.start(modelrpc.Request{RequestID: strings.Repeat("a", 32), SessionID: "run"})
		}()
		go func() {
			<-start
			bridge.close()
			close(closed)
		}()
		close(start)
		if err := <-admitted; err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		<-closed
		bridge.mu.Lock()
		remaining := len(bridge.active)
		bridge.mu.Unlock()
		if remaining != 0 {
			bridge.close()
			t.Fatal("close returned while an admitted model call was still active")
		}
	}
}

func unicodeSecret(value string) string {
	var out strings.Builder
	for _, ch := range value {
		fmt.Fprintf(&out, `\u%04x`, ch)
	}
	return out.String()
}
