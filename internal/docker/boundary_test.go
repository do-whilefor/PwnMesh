package docker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"pwnmesh/internal/config"
)

func TestEnsureRequiresCleanModelBoundaryBeforeReusingContainer(t *testing.T) {
	const secret = "sentinel-must-not-appear-in-errors"
	for _, tc := range []struct {
		name, boundary string
		env            []string
		allowed        bool
	}{
		{name: "legacy container"},
		{name: "different boundary", boundary: "worker-v0"},
		{name: "auth token", boundary: "dispatcher-v1", env: []string{"ANTHROPIC_AUTH_TOKEN=" + secret}},
		{name: "API key", boundary: "dispatcher-v1", env: []string{"ANTHROPIC_API_KEY=" + secret}},
		{name: "URL credentials", boundary: "dispatcher-v1", env: []string{"ANTHROPIC_BASE_URL=https://" + secret + "@model.invalid"}},
		{name: "provider setting", boundary: "dispatcher-v1", env: []string{"ANTHROPIC_MODEL=fixture"}},
		{name: "lowercase key", boundary: "dispatcher-v1", env: []string{"anthropic_auth_token=" + secret}},
		{name: "empty credentials", boundary: "dispatcher-v1", env: []string{"ANTHROPIC_AUTH_TOKEN=", "ANTHROPIC_API_KEY=", "PATH=/usr/bin"}, allowed: true},
		{name: "clean environment", boundary: "dispatcher-v1", env: []string{"LANG=C", "PWNMESH_CONTEXT_BYTES=8388608"}, allowed: true},
	} {
		for _, running := range []bool{true, false} {
			t.Run(tc.name+"/"+map[bool]string{true: "running", false: "stopped"}[running], func(t *testing.T) {
				starts := 0
				engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch {
					case r.Method == http.MethodGet && r.URL.Path == "/containers/test-dispatch-p/json":
						_ = json.NewEncoder(w).Encode(map[string]any{
							"Image": "sha256:worker", "Config": map[string]any{"Env": tc.env, "Labels": map[string]string{
								"pwnmesh.namespace": "test", "pwnmesh.project": "p", "pwnmesh.model-boundary": tc.boundary,
							}}, "State": map[string]bool{"Running": running}, "HostConfig": map[string]string{"NetworkMode": "bridge"},
						})
					case r.Method == http.MethodGet && r.URL.Path == "/images/worker/json":
						_ = json.NewEncoder(w).Encode(map[string]string{"Id": "sha256:worker"})
					case r.Method == http.MethodPost && r.URL.Path == "/containers/test-dispatch-p/start":
						starts++
					default:
						t.Errorf("unexpected engine request: %s %s", r.Method, r.URL.Path)
						w.WriteHeader(http.StatusInternalServerError)
					}
				}))
				defer engine.Close()
				client := boundaryClient(engine.URL)
				name, err := client.ensure(context.Background(), "p")
				if !tc.allowed {
					if err == nil || name != "" || starts != 0 || !strings.Contains(err.Error(), "preserve and migrate its workspace") || strings.Contains(err.Error(), secret) {
						t.Fatalf("unsafe container accepted, changed or leaked credentials: name=%q err=%v starts=%d", name, err, starts)
					}
					return
				}
				wantStarts := 0
				if !running {
					wantStarts = 1
				}
				if err != nil || name != "test-dispatch-p" || starts != wantStarts {
					t.Fatalf("clean container rejected: name=%q err=%v starts=%d", name, err, starts)
				}
			})
		}
	}
}

func TestEnsureRejectsModelEnvironmentInImageBeforeAnyMutation(t *testing.T) {
	const secret = "image-secret-sentinel"
	for _, key := range []string{"ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_API_KEY", "ANTHROPIC_BASE_URL", "ANTHROPIC_MODEL"} {
		for _, existing := range []bool{true, false} {
			t.Run(key+"/"+map[bool]string{true: "existing", false: "new"}[existing], func(t *testing.T) {
				engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch {
					case r.Method == http.MethodGet && r.URL.Path == "/containers/test-dispatch-p/json":
						if !existing {
							w.WriteHeader(http.StatusNotFound)
							return
						}
						_ = json.NewEncoder(w).Encode(map[string]any{
							"Image": "sha256:worker", "Config": map[string]any{"Labels": map[string]string{
								"pwnmesh.namespace": "test", "pwnmesh.project": "p", "pwnmesh.model-boundary": "dispatcher-v1",
							}}, "State": map[string]bool{"Running": false}, "HostConfig": map[string]string{"NetworkMode": "bridge"},
						})
					case r.Method == http.MethodGet && r.URL.Path == "/images/worker/json":
						_ = json.NewEncoder(w).Encode(map[string]any{"Id": "sha256:worker", "Config": map[string]any{"Env": []string{key + "=" + secret}}})
					default:
						t.Errorf("image rejection must precede mutations: %s %s", r.Method, r.URL.Path)
						w.WriteHeader(http.StatusInternalServerError)
					}
				}))
				defer engine.Close()
				name, err := boundaryClient(engine.URL).ensure(context.Background(), "p")
				if err == nil || name != "" || !strings.Contains(err.Error(), "worker image contains model provider environment") || strings.Contains(err.Error(), secret) {
					t.Fatalf("unsafe image accepted or leaked credentials: name=%q err=%v", name, err)
				}
			})
		}
	}
}

func TestEnsureCreatesBoundaryAndRechecksCreateRaces(t *testing.T) {
	for _, tc := range []struct {
		name, boundary string
		race           bool
		env            []string
		allowed        bool
	}{
		{name: "new container", allowed: true},
		{name: "clean raced container", race: true, boundary: "dispatcher-v1", allowed: true},
		{name: "legacy raced container", race: true},
		{name: "credentialed raced container", race: true, boundary: "dispatcher-v1", env: []string{"ANTHROPIC_AUTH_TOKEN=raced-secret"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inspects, creates, starts := 0, 0, 0
			engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/containers/test-dispatch-p/json":
					inspects++
					if inspects == 1 {
						w.WriteHeader(http.StatusNotFound)
						return
					}
					_ = json.NewEncoder(w).Encode(map[string]any{
						"Image": "sha256:worker", "Config": map[string]any{"Env": tc.env, "Labels": map[string]string{
							"pwnmesh.namespace": "test", "pwnmesh.project": "p", "pwnmesh.model-boundary": tc.boundary,
						}}, "State": map[string]bool{"Running": false}, "HostConfig": map[string]string{"NetworkMode": "bridge"},
					})
				case r.Method == http.MethodGet && r.URL.Path == "/images/worker/json":
					_ = json.NewEncoder(w).Encode(map[string]any{"Id": "sha256:worker", "Config": map[string]any{"Env": []string{"ANTHROPIC_AUTH_TOKEN=", "PATH=/usr/bin"}}})
				case r.Method == http.MethodPost && r.URL.Path == "/containers/create":
					creates++
					var input struct {
						Image      string
						Labels     map[string]string
						HostConfig map[string]any
					}
					if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
						t.Error(err)
					}
					if input.Image != "sha256:worker" || input.Labels["pwnmesh.model-boundary"] != "dispatcher-v1" || input.Labels["pwnmesh.namespace"] != "test" || input.Labels["pwnmesh.project"] != "p" {
						t.Errorf("incorrect creation identity: %+v", input)
					}
					for _, key := range []string{"PidMode", "Privileged", "Binds", "Mounts"} {
						if _, present := input.HostConfig[key]; present {
							t.Errorf("model boundary introduced host access: %s", key)
						}
					}
					if tc.race {
						w.WriteHeader(http.StatusConflict)
					}
				case r.Method == http.MethodPost && r.URL.Path == "/containers/test-dispatch-p/start":
					starts++
				default:
					t.Errorf("unexpected engine request: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusInternalServerError)
				}
			}))
			defer engine.Close()
			name, err := boundaryClient(engine.URL).ensure(context.Background(), "p")
			if creates != 1 || (tc.race && inspects != 2) {
				t.Fatalf("creation/race was not exercised: creates=%d inspects=%d", creates, inspects)
			}
			if tc.allowed {
				if err != nil || name != "test-dispatch-p" || starts != 1 {
					t.Fatalf("clean creation failed: name=%q err=%v starts=%d", name, err, starts)
				}
			} else if err == nil || name != "" || starts != 0 || !strings.Contains(err.Error(), "preserve and migrate its workspace") || strings.Contains(err.Error(), "raced-secret") {
				t.Fatalf("unsafe raced container accepted or leaked credentials: name=%q err=%v starts=%d", name, err, starts)
			}
		})
	}
}

func boundaryClient(endpoint string) *Client {
	return &Client{Config: config.Container{Namespace: "test", Image: "worker", Network: "bridge"}, http: &http.Client{Transport: graphBridgeTestTransport{base: http.DefaultTransport, endpoint: endpoint}}}
}
