package docker

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"pwnmesh/internal/config"
)

func TestWorkerProxyAppliesToEveryToolWithoutProviderSecret(t *testing.T) {
	for _, mode := range []string{"direct", "proxy"} {
		w := config.Worker{Env: map[string]string{"ANTHROPIC_AUTH_TOKEN": "fixture-secret", "ANTHROPIC_BASE_URL": "https://model.invalid", "PWNMESH_CONNECTION_MODE": mode, "PWNMESH_PROXY_URL": "http://localhost:7897", "HTTPS_PROXY": "http://stale.invalid:1234", "http_proxy": "http://stale.invalid:1234"}}
		env, err := modelWorkerEnv(w, "fixture-secret")
		if err != nil {
			t.Fatal(err)
		}
		values := map[string]string{}
		for _, entry := range env {
			key, value, _ := strings.Cut(entry, "=")
			if _, duplicate := values[key]; duplicate {
				t.Fatalf("duplicate environment %s", key)
			}
			values[key] = value
			if strings.Contains(entry, "fixture-secret") || strings.HasPrefix(entry, "ANTHROPIC_") {
				t.Fatal("worker received model credentials")
			}
		}
		want := ""
		if mode == "proxy" {
			want = "http://host.docker.internal:7897"
		}
		for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
			if value, ok := values[key]; !ok || value != want {
				t.Errorf("%s %s did not route correctly", mode, key)
			}
		}
		if values["NO_PROXY"] != "localhost,127.0.0.1,::1,server" {
			t.Fatal("local worker services not excluded")
		}
	}
}

func TestProxyUpgradesExistingContainerHostAliasWithoutWorkspaceMutation(t *testing.T) {
	for _, mode := range []string{"configured", "dns", "missing", "lookup-failure"} {
		t.Run(mode, func(t *testing.T) {
			execs, lookups := 0, 0
			engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == "GET" && r.URL.Path == "/containers/project/json":
					hosts := []string{}
					if mode == "configured" {
						hosts = append(hosts, "host.docker.internal:host-gateway")
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"HostConfig": map[string]any{"ExtraHosts": hosts}})
				case r.Method == "POST" && r.URL.Path == "/containers/project/exec":
					execs++
					var body struct {
						Cmd []string `json:"Cmd"`
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					if execs == 1 && strings.Join(body.Cmd, " ") != "getent hosts host.docker.internal" {
						t.Error("unexpected initial DNS check")
					}
					if execs == 2 && (len(body.Cmd) != 5 || body.Cmd[4] != "192.0.2.10" || strings.Contains(body.Cmd[2], "/workspace") || !strings.Contains(body.Cmd[2], "/etc/hosts")) {
						t.Error("unsafe migration command")
					}
					_ = json.NewEncoder(w).Encode(map[string]string{"Id": "host-check"})
				case r.URL.Path == "/exec/host-check/start":
				case r.URL.Path == "/exec/host-check/json":
					code := 0
					if execs == 1 && mode != "dns" {
						code = 2
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"Running": false, "ExitCode": code})
				default:
					t.Errorf("unexpected container mutation %s %s", r.Method, r.URL.Path)
					w.WriteHeader(500)
				}
			}))
			defer engine.Close()
			client := &Client{http: &http.Client{Transport: graphBridgeTestTransport{base: http.DefaultTransport, endpoint: engine.URL}}, lookupProxyHost: func(context.Context, string) ([]net.IPAddr, error) {
				lookups++
				if mode == "lookup-failure" {
					return nil, errors.New("unavailable")
				}
				return []net.IPAddr{{IP: net.ParseIP("192.0.2.10")}}, nil
			}}
			err := client.ensureProxyHost(context.Background(), "project", "http://localhost:7897")
			if (err != nil) != (mode == "lookup-failure") {
				t.Fatalf("migration outcome: %v", err)
			}
			want := map[string]int{"configured": 0, "dns": 1, "missing": 2, "lookup-failure": 1}[mode]
			if execs != want || (lookups != 0) != (mode == "missing" || mode == "lookup-failure") {
				t.Fatalf("execs=%d lookups=%d", execs, lookups)
			}
		})
	}
}
