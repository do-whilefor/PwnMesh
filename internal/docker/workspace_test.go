package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type workspaceEngine struct {
	t                                                             *testing.T
	client                                                        *Client
	container                                                     map[string]any
	volumes                                                       map[string]map[string]any
	creates, starts, stops, removes, volumeCreates, volumeRemoves int
	volumeBusy                                                    bool
	volumeRace                                                    func(map[string]any)
	containerRace                                                 func(map[string]any)
	imageID, sharedNetworkID                                      string
}

func newWorkspaceEngine(t *testing.T) (*Client, *workspaceEngine) {
	t.Helper()
	e := &workspaceEngine{t: t, volumes: make(map[string]map[string]any), imageID: "sha256:worker"}
	server := httptest.NewServer(http.HandlerFunc(e.serve))
	t.Cleanup(server.Close)
	c := boundaryClient(server.URL)
	c.Config.CompletedAction = "remove"
	if err := c.BindOwner(strings.Repeat("a", 32)); err != nil {
		t.Fatal(err)
	}
	e.client = c
	return c, e
}

func (e *workspaceEngine) serve(w http.ResponseWriter, r *http.Request) {
	write := func(value any) { _ = json.NewEncoder(w).Encode(value) }
	decode := func() map[string]any {
		var value map[string]any
		if err := json.NewDecoder(r.Body).Decode(&value); err != nil {
			e.t.Error(err)
		}
		return value
	}
	switch {
	case r.Method == "GET" && r.URL.Path == "/images/worker/json":
		write(map[string]string{"Id": e.imageID})
	case r.Method == "GET" && r.URL.Path == "/containers/network-owner/json":
		write(map[string]string{"Id": e.sharedNetworkID})
	case r.Method == "GET" && r.URL.Path == "/containers/json":
		items := []any{}
		if e.container != nil {
			items = append(items, map[string]any{"Labels": e.container["Config"].(map[string]any)["Labels"]})
		}
		write(items)
	case r.Method == "GET" && r.URL.Path == "/containers/"+e.client.name("p")+"/json":
		if e.container == nil {
			w.WriteHeader(404)
		} else {
			write(e.container)
		}
	case r.Method == "GET" && r.URL.Path == "/containers/test-dispatch-p/json":
		w.WriteHeader(404)
	case r.Method == "POST" && r.URL.Path == "/containers/create":
		e.creates++
		input := decode()
		host := input["HostConfig"].(map[string]any)
		mounts := []any{}
		if raw, ok := host["Mounts"].([]any); ok {
			for _, item := range raw {
				mount := item.(map[string]any)
				if options, ok := mount["VolumeOptions"].(map[string]any); ok && options["NoCopy"] == true {
					e.t.Error("fresh workspace disabled image copy-up")
				}
				mounts = append(mounts, map[string]any{"Type": mount["Type"], "Name": mount["Source"], "Destination": mount["Target"], "RW": true})
			}
		}
		e.container = map[string]any{"Id": fmt.Sprintf("container-%d", e.creates), "Image": input["Image"], "Config": map[string]any{"Labels": input["Labels"]}, "HostConfig": host, "Mounts": mounts, "State": map[string]bool{"Running": false}}
		if e.containerRace != nil {
			e.containerRace(e.container)
			w.WriteHeader(409)
		} else {
			write(map[string]string{"Id": e.container["Id"].(string)})
		}
	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/start"):
		e.starts++
	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/stop"):
		e.stops++
	case r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, "/containers/"):
		if e.container == nil || r.URL.Path != "/containers/"+e.container["Id"].(string) {
			e.t.Error("cleanup did not use immutable container ID")
		}
		e.removes++
		e.container = nil
	case r.Method == "GET" && r.URL.Path == "/volumes":
		if !strings.Contains(r.URL.Query().Get("filters"), e.client.ownerID()) || !strings.Contains(r.URL.Query().Get("filters"), "workspace-v1") {
			e.t.Error("volume listing omitted ownership/resource filters")
		}
		items := []any{}
		for _, volume := range e.volumes {
			items = append(items, volume)
		}
		write(map[string]any{"Volumes": items})
	case r.Method == "POST" && r.URL.Path == "/volumes/create":
		e.volumeCreates++
		input := decode()
		if e.volumeRace != nil {
			e.volumeRace(input)
		}
		e.volumes[input["Name"].(string)] = input
		write(input)
	case strings.HasPrefix(r.URL.Path, "/volumes/"):
		name := strings.TrimPrefix(r.URL.Path, "/volumes/")
		volume := e.volumes[name]
		if volume == nil {
			w.WriteHeader(404)
			return
		}
		if r.Method == "GET" {
			write(volume)
			return
		}
		if r.Method == "DELETE" {
			if r.URL.Query().Get("force") == "true" {
				e.t.Error("forced volume deletion bypasses use protection")
			}
			if e.volumeBusy {
				w.WriteHeader(409)
				return
			}
			e.volumeRemoves++
			delete(e.volumes, name)
			return
		}
		fallthrough
	default:
		e.t.Errorf("unexpected engine request: %s %s", r.Method, r.URL)
		w.WriteHeader(500)
	}
}

func TestWorkspaceSurvivesCompletionAndRecreation(t *testing.T) {
	c, e := newWorkspaceEngine(t)
	ctx := context.Background()
	if _, err := c.ensure(ctx, "p"); err != nil {
		t.Fatal(err)
	}
	if e.volumeCreates != 1 || len(e.container["Mounts"].([]any)) != 1 {
		t.Fatal("workspace was created in the disposable container layer")
	}
	var name string
	for key, volume := range e.volumes {
		name = key
		volume["evidence"] = "saved proof"
	}
	if err := c.Cleanup(ctx, "p", "completed"); err != nil {
		t.Fatal(err)
	}
	if e.container != nil || len(e.volumes) != 1 {
		t.Fatal("completion did not retain workspace independently")
	}
	projects, err := c.Projects(ctx)
	if err != nil || len(projects) != 1 || projects[0] != "p" {
		t.Fatalf("volume-only project disappeared: %v %v", projects, err)
	}
	if err := c.Cleanup(ctx, "p", "stopped"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ensure(ctx, "p"); err != nil {
		t.Fatal(err)
	}
	if e.volumeCreates != 1 || e.volumes[name]["evidence"] != "saved proof" {
		t.Fatal("recreation replaced saved workspace")
	}
	if err := c.Cleanup(ctx, "p", "deleted"); err != nil {
		t.Fatal(err)
	}
	if e.container != nil || len(e.volumes) != 0 || e.volumeRemoves != 1 {
		t.Fatal("explicit deletion leaked workspace")
	}
}

func TestWorkspaceDeletionRetriesWithoutContainer(t *testing.T) {
	c, e := newWorkspaceEngine(t)
	ctx := context.Background()
	if _, err := c.ensure(ctx, "p"); err != nil {
		t.Fatal(err)
	}
	e.volumeBusy = true
	if err := c.Cleanup(ctx, "p", "deleted"); err == nil {
		t.Fatal("in-use volume deletion was accepted")
	}
	if e.container != nil || len(e.volumes) != 1 {
		t.Fatal("failed cleanup lost retry identity")
	}
	projects, err := c.Projects(ctx)
	if err != nil || len(projects) != 1 {
		t.Fatalf("orphan cleanup cannot retry: %v %v", projects, err)
	}
	e.volumeBusy = false
	if err := c.Cleanup(ctx, "p", "deleted"); err != nil {
		t.Fatal(err)
	}
	if len(e.volumes) != 0 {
		t.Fatal("volume-only deletion leaked workspace")
	}
}

func TestWorkspaceRejectsForeignOrUnexpectedVolumes(t *testing.T) {
	for _, field := range []string{"pwnmesh.installation", "pwnmesh.namespace", "pwnmesh.project", "pwnmesh.resource", "Name", "Driver", "Options"} {
		t.Run(field, func(t *testing.T) {
			c, e := newWorkspaceEngine(t)
			e.volumeRace = func(volume map[string]any) {
				switch field {
				case "Name", "Driver":
					volume[field] = "foreign"
				case "Options":
					volume[field] = map[string]string{"type": "none", "o": "bind", "device": "/host"}
				default:
					volume["Labels"].(map[string]any)[field] = "foreign"
				}
			}
			if _, err := c.ensure(context.Background(), "p"); err == nil {
				t.Fatal("unsafe volume creation race was adopted")
			}
			if e.creates != 0 || e.starts != 0 || e.volumeRemoves != 0 {
				t.Fatal("unsafe workspace was used or deleted")
			}
			_ = c.Cleanup(context.Background(), "p", "deleted")
			if e.volumeRemoves != 0 {
				t.Fatal("unverified volume was deleted")
			}
		})
	}
}

func TestWorkspaceRejectsUnexpectedContainerMounts(t *testing.T) {
	for _, field := range []string{"Type", "Name", "Destination", "RW", "nested", "marker", "missing"} {
		t.Run(field, func(t *testing.T) {
			c, e := newWorkspaceEngine(t)
			ctx := context.Background()
			if _, err := c.ensure(ctx, "p"); err != nil {
				t.Fatal(err)
			}
			mounts := e.container["Mounts"].([]any)
			mount := mounts[0].(map[string]any)
			switch field {
			case "RW":
				mount[field] = false
			case "nested":
				e.container["Mounts"] = append(mounts, map[string]any{"Type": "bind", "Destination": "/workspace/evidence", "RW": true})
			case "marker":
				delete(e.container["Config"].(map[string]any)["Labels"].(map[string]any), workspaceVolumeLabel)
			case "missing":
				e.container["Mounts"] = []any{}
			default:
				mount[field] = "unexpected"
			}
			if _, err := c.ensure(ctx, "p"); err == nil {
				t.Fatal("unexpected mounted workspace was reused")
			}
			if err := c.Cleanup(ctx, "p", "deleted"); err == nil {
				t.Fatal("unexpected mounted workspace was deleted")
			}
			if e.starts != 1 || e.stops != 0 || e.removes != 0 || e.volumeRemoves != 0 {
				t.Fatal("unsafe container or workspace was mutated")
			}
		})
	}
}

func TestWorkspaceRechecksContainerCreationRaces(t *testing.T) {
	for _, unsafe := range []bool{false, true} {
		t.Run(fmt.Sprint(unsafe), func(t *testing.T) {
			c, e := newWorkspaceEngine(t)
			e.containerRace = func(container map[string]any) {
				if unsafe {
					container["Mounts"].([]any)[0].(map[string]any)["Name"] = "foreign-workspace"
				}
			}
			_, err := c.ensure(context.Background(), "p")
			if (err != nil) != unsafe {
				t.Fatalf("unexpected race result: %v", err)
			}
			if e.starts != map[bool]int{true: 0, false: 1}[unsafe] {
				t.Fatal("raced mount was not checked before start")
			}
		})
	}
}

func TestWorkspaceRuntimeBindingSurvivesContainerRemoval(t *testing.T) {
	for _, changed := range []string{"image", "network", "capabilities"} {
		t.Run(changed, func(t *testing.T) {
			c, e := newWorkspaceEngine(t)
			ctx := context.Background()
			if _, err := c.ensure(ctx, "p"); err != nil {
				t.Fatal(err)
			}
			if err := c.Cleanup(ctx, "p", "completed"); err != nil {
				t.Fatal(err)
			}
			switch changed {
			case "image":
				e.imageID = "sha256:replacement"
			case "network":
				c.Config.Network = "host"
			case "capabilities":
				c.Config.CapAdd = []string{"NET_ADMIN"}
			}
			if _, err := c.ensure(ctx, "p"); err == nil || !strings.Contains(err.Error(), "different image, network, or capabilities") {
				t.Fatalf("retained workspace lost runtime binding: %v", err)
			}
			if e.creates != 1 || e.volumeCreates != 1 || e.volumeRemoves != 0 {
				t.Fatal("runtime drift changed the saved workspace")
			}
		})
	}
}

func TestWorkspaceBindsSharedNetworkToImmutableTarget(t *testing.T) {
	c, e := newWorkspaceEngine(t)
	c.Config.Network = "container:network-owner"
	e.sharedNetworkID = strings.Repeat("b", 64)
	ctx := context.Background()
	if _, err := c.ensure(ctx, "p"); err != nil {
		t.Fatal(err)
	}
	if actual := e.container["HostConfig"].(map[string]any)["NetworkMode"]; actual != "container:"+e.sharedNetworkID {
		t.Fatalf("creation retained a mutable alias: %v", actual)
	}
	if err := c.Cleanup(ctx, "p", "completed"); err != nil {
		t.Fatal(err)
	}
	e.sharedNetworkID = strings.Repeat("c", 64)
	if _, err := c.ensure(ctx, "p"); err == nil {
		t.Fatal("rebound shared-network alias was accepted")
	}
}

func TestWorkspaceListingRevalidatesOwnershipAndDeduplicates(t *testing.T) {
	c, e := newWorkspaceEngine(t)
	ctx := context.Background()
	if _, err := c.ensure(ctx, "p"); err != nil {
		t.Fatal(err)
	}
	projects, err := c.Projects(ctx)
	if err != nil || len(projects) != 1 || projects[0] != "p" {
		t.Fatalf("container and volume were not deduplicated: %v %v", projects, err)
	}
	e.container = nil
	for _, volume := range e.volumes {
		volume["Labels"].(map[string]any)["pwnmesh.installation"] = strings.Repeat("b", 32)
	}
	projects, err = c.Projects(ctx)
	if err != nil || len(projects) != 0 {
		t.Fatalf("foreign volume was listed: %v %v", projects, err)
	}
	if err := c.Cleanup(ctx, "p", "deleted"); err == nil || e.volumeRemoves != 0 {
		t.Fatal("foreign orphan volume was deleted")
	}
}

func TestLegacyWorkspaceIsPreservedUntilProjectDeletion(t *testing.T) {
	c, e := newWorkspaceEngine(t)
	e.container = map[string]any{"Id": "legacy", "Image": "sha256:worker", "Config": map[string]any{"Labels": c.labels("p", "dispatcher-v1")}, "HostConfig": map[string]string{"NetworkMode": "bridge"}, "State": map[string]bool{"Running": true}}
	if _, err := c.ensure(context.Background(), "p"); err != nil {
		t.Fatal(err)
	}
	if err := c.Cleanup(context.Background(), "p", "completed"); err != nil {
		t.Fatal(err)
	}
	if e.container == nil || e.stops != 1 || e.volumeCreates != 0 {
		t.Fatal("legacy workspace was deleted or hidden by a new volume")
	}
	if err := c.Cleanup(context.Background(), "p", "deleted"); err != nil {
		t.Fatal(err)
	}
	if e.container != nil {
		t.Fatal("explicit deletion did not remove legacy container")
	}
}
