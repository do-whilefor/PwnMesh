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

func TestInstallationOwnershipRejectsForeignContainers(t *testing.T) {
	owner, foreign := strings.Repeat("a", 32), strings.Repeat("b", 32)
	mutations := 0
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			mutations++
			w.WriteHeader(500)
			return
		}
		labels := map[string]string{"pwnmesh.namespace": "test", "pwnmesh.project": "p", "pwnmesh.installation": foreign}
		if r.URL.Path == "/containers/json" {
			var filters map[string][]string
			if err := json.Unmarshal([]byte(r.URL.Query().Get("filters")), &filters); err != nil {
				t.Error(err)
			}
			if !strings.Contains(r.URL.Query().Get("filters"), owner) {
				t.Error("listing did not filter installation")
			}
			// Verify labels again even if the Engine returned an unexpected entry.
			_ = json.NewEncoder(w).Encode([]any{map[string]any{"Labels": labels}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"Id": "foreign-container", "Config": map[string]any{"Labels": labels}})
	}))
	defer engine.Close()
	c := boundaryClient(engine.URL)
	if err := c.BindOwner(owner); err != nil {
		t.Fatal(err)
	}
	if c.ContainerName("p") == "test-dispatch-p" {
		t.Fatal("container name lacks installation")
	}
	projects, err := c.Projects(context.Background())
	if err != nil || len(projects) != 0 {
		t.Fatalf("foreign projects returned: %v %v", projects, err)
	}
	if err := c.Cleanup(context.Background(), "p", "deleted"); err == nil {
		t.Fatal("foreign container deletion accepted")
	}
	if _, err := c.ensure(context.Background(), "p"); err == nil {
		t.Fatal("foreign container reuse accepted")
	}
	if mutations != 0 {
		t.Fatal("foreign container was changed")
	}
	if err := c.BindOwner(foreign); err == nil {
		t.Fatal("live database switch accepted")
	}
}

func TestInstallationCreationLabelsAndLegacyPreservation(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "fresh", true: "legacy"}[legacy], func(t *testing.T) {
			owner := strings.Repeat("a", 32)
			mutations := 0
			engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/images/worker/json":
					_ = json.NewEncoder(w).Encode(map[string]string{"Id": "sha256:worker"})
				case r.Method == "GET":
					if legacy && r.URL.Path == "/containers/test-dispatch-p/json" {
						_ = json.NewEncoder(w).Encode(map[string]any{"Config": map[string]any{"Labels": map[string]string{"pwnmesh.namespace": "test", "pwnmesh.project": "p"}}})
					} else {
						w.WriteHeader(404)
					}
				case r.URL.Path == "/containers/create":
					mutations++
					var input struct{ Labels map[string]string }
					if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
						t.Error(err)
					}
					if input.Labels["pwnmesh.installation"] != owner || r.URL.Query().Get("name") != "test-"+owner+"-dispatch-p" {
						t.Errorf("unowned creation: %+v %s", input, r.URL)
					}
				case strings.HasSuffix(r.URL.Path, "/start"):
					mutations++
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL)
					w.WriteHeader(500)
				}
			}))
			defer engine.Close()
			c := boundaryClient(engine.URL)
			if err := c.BindOwner(owner); err != nil {
				t.Fatal(err)
			}
			_, err := c.ensure(context.Background(), "p")
			if legacy {
				if err == nil || !strings.Contains(err.Error(), "legacy container") || mutations != 0 {
					t.Fatalf("legacy workspace was not preserved: %v, mutations=%d", err, mutations)
				}
			} else if err != nil || mutations != 2 {
				t.Fatalf("creation failed: %v, mutations=%d", err, mutations)
			}
		})
	}
}

func TestNewContainerRunnerRequiresOwnership(t *testing.T) {
	c := New(config.Container{Namespace: "test"})
	defer c.Close()
	if _, err := c.Projects(context.Background()); err == nil {
		t.Fatal("unbound runner listed containers")
	}
	if err := c.Cleanup(context.Background(), "p", "deleted"); err == nil {
		t.Fatal("unbound runner cleaned containers")
	}
}
