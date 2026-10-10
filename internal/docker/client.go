// Package docker uses the Docker Engine HTTP API through its Unix socket.
package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"pwnmesh/internal/board"
	"pwnmesh/internal/config"
	"pwnmesh/internal/modelrpc"
	"pwnmesh/internal/worker"
)

type Client struct {
	Config          config.Container
	http            *http.Client
	locks           sync.Map
	graphMu         sync.RWMutex
	graphHandler    func(context.Context, worker.Job, worker.GraphRequest) (any, error)
	inputReader     func(context.Context, worker.Job, board.InputFile) (io.ReadCloser, error)
	lookupProxyHost func(context.Context, string) ([]net.IPAddr, error)
}
type APIError struct{ Status int }

func (e *APIError) Error() string { return fmt.Sprintf("Docker Engine returned HTTP %d", e.Status) }
func New(c config.Container) *Client {
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", c.Socket)
	}, ResponseHeaderTimeout: 30 * time.Second}
	return &Client{Config: c, http: &http.Client{Transport: transport}}
}
func (c *Client) Close()                { c.http.CloseIdleConnections() }
func (c *Client) name(id string) string { return c.Config.Namespace + "-dispatch-" + id }
func (c *Client) lock(id string) func() {
	value, _ := c.locks.LoadOrStore(id, &sync.Mutex{})
	mu := value.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}
func (c *Client) request(ctx context.Context, method, route string, body io.Reader, content string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, "http://docker"+route, body)
	if err != nil {
		return nil, err
	}
	if content != "" {
		req.Header.Set("Content-Type", content)
	}
	res, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode >= 300 {
		res.Body.Close()
		return nil, &APIError{res.StatusCode}
	}
	return res, nil
}
func (c *Client) json(ctx context.Context, method, route string, input, output any) error {
	var body io.Reader
	if input != nil {
		raw, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	res, err := c.request(ctx, method, route, body, "application/json")
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if output != nil {
		return json.NewDecoder(io.LimitReader(res.Body, 16<<20)).Decode(output)
	}
	_, err = io.Copy(io.Discard, res.Body)
	return err
}
func status(err error, code int) bool { var e *APIError; return errors.As(err, &e) && e.Status == code }
func (c *Client) ensure(ctx context.Context, id string, secrets ...string) (string, error) {
	unlock := c.lock(id)
	defer unlock()
	name := c.name(id)
	const modelBoundary = "dispatcher-v1"
	hasModelEnvironment := func(env []string) bool {
		for _, entry := range env {
			key, value, _ := strings.Cut(entry, "=")
			if (strings.HasPrefix(strings.ToUpper(key), "ANTHROPIC_") && value != "") || containsModelCredential(entry, secrets...) {
				return true
			}
		}
		return false
	}
	var info struct {
		Image  string `json:"Image"`
		Config struct {
			Labels map[string]string `json:"Labels"`
			Env    []string          `json:"Env"`
		} `json:"Config"`
		State struct {
			Running bool `json:"Running"`
		} `json:"State"`
		HostConfig struct {
			NetworkMode string   `json:"NetworkMode"`
			CapAdd      []string `json:"CapAdd"`
		} `json:"HostConfig"`
	}
	err := c.json(ctx, "GET", "/containers/"+url.PathEscape(name)+"/json", nil, &info)
	if err == nil && (info.Config.Labels["pwnmesh.namespace"] != c.Config.Namespace || info.Config.Labels["pwnmesh.project"] != id) {
		return "", errors.New("container name belongs to a different project or dispatcher namespace")
	}
	missing := status(err, 404)
	if err != nil && !missing {
		return "", err
	}
	checkBoundary := func() error {
		if info.Config.Labels["pwnmesh.model-boundary"] != modelBoundary {
			return fmt.Errorf("project %s container predates or differs from the dispatcher model boundary; preserve and migrate its workspace before recreating the container", id)
		}
		if hasModelEnvironment(info.Config.Env) {
			return fmt.Errorf("project %s container contains model provider environment; preserve and migrate its workspace before recreating the container without ANTHROPIC_ environment", id)
		}
		return nil
	}
	if !missing {
		if err := checkBoundary(); err != nil {
			return "", err
		}
	}
	// A mutable tag may now name a different binary or runtime. Resolve it on
	// every launch and compare immutable IDs before touching a saved workspace.
	var desired struct {
		ID     string `json:"Id"`
		Config struct {
			Env []string `json:"Env"`
		} `json:"Config"`
	}
	if err = c.json(ctx, "GET", "/images/"+url.PathEscape(c.Config.Image)+"/json", nil, &desired); err != nil {
		return "", fmt.Errorf("resolve configured worker image %q locally: %w", c.Config.Image, err)
	}
	if desired.ID == "" {
		return "", errors.New("Docker returned an empty ID for the configured worker image")
	}
	if hasModelEnvironment(desired.Config.Env) {
		return "", errors.New("worker image contains model provider environment; rebuild it without ANTHROPIC_ environment and keep model configuration in the dispatcher")
	}
	if missing {
		hostConfig := map[string]any{"NetworkMode": c.Config.Network, "CapAdd": c.Config.CapAdd, "Init": true}
		// Docker rejects ExtraHosts with container:<id> networking. That mode
		// shares the target container's hosts file, so its owner supplies any
		// host.docker.internal mapping needed by a proxy.
		if !strings.HasPrefix(c.Config.Network, "container:") {
			hostConfig["ExtraHosts"] = []string{"host.docker.internal:host-gateway"}
		}
		input := map[string]any{"Image": desired.ID, "Entrypoint": []string{"/bin/sh", "-c"}, "Cmd": []string{"exec sleep infinity"}, "WorkingDir": "/workspace", "Labels": map[string]string{"pwnmesh.namespace": c.Config.Namespace, "pwnmesh.project": id, "pwnmesh.model-boundary": modelBoundary}, "HostConfig": hostConfig}
		err = c.json(ctx, "POST", "/containers/create?name="+url.QueryEscape(name), input, nil)
		if status(err, 409) {
			missing = false
			err = c.json(ctx, "GET", "/containers/"+url.PathEscape(name)+"/json", nil, &info)
			if err == nil && (info.Config.Labels["pwnmesh.namespace"] != c.Config.Namespace || info.Config.Labels["pwnmesh.project"] != id) {
				return "", errors.New("container creation raced with a different project or dispatcher namespace")
			}
			if err == nil {
				err = checkBoundary()
			}
		}
	}
	if err != nil {
		return "", err
	}
	if !missing && info.Image != desired.ID {
		return "", fmt.Errorf("project %s container uses image %q but configured worker image %q resolves to %q; preserve and migrate its workspace before recreating the container", id, info.Image, c.Config.Image, desired.ID)
	}
	if !missing && (networkMode(info.HostConfig.NetworkMode) != networkMode(c.Config.Network) || !slices.Equal(capabilities(info.HostConfig.CapAdd), capabilities(c.Config.CapAdd))) {
		return "", fmt.Errorf("project %s container uses network %q and added capabilities %v but configured network is %q and added capabilities are %v; preserve and migrate its workspace before recreating the container", id, info.HostConfig.NetworkMode, info.HostConfig.CapAdd, c.Config.Network, c.Config.CapAdd)
	}
	if !info.State.Running {
		err = c.json(ctx, "POST", "/containers/"+url.PathEscape(name)+"/start", nil, nil)
		if status(err, 304) {
			err = nil
		}
	}
	return name, err
}

// Docker's Linux default network is bridge. Capability names are insensitive
// to order, case and the optional CAP_ prefix; ALL subsumes individual names.
func networkMode(value string) string {
	if value == "" || value == "default" {
		return "bridge"
	}
	return value
}

func capabilities(values []string) []string {
	set := map[string]bool{}
	for _, value := range values {
		value = strings.ToUpper(value)
		if value == "ALL" {
			return []string{"ALL"}
		}
		if !strings.HasPrefix(value, "CAP_") {
			value = "CAP_" + value
		}
		set[value] = true
	}
	names := make([]string, 0, len(set))
	for value := range set {
		names = append(names, value)
	}
	sort.Strings(names)
	return names
}

func (c *Client) archive(ctx context.Context, name, target string, data []byte) error {
	if !strings.HasPrefix(target, "/workspace/.pwnmesh/runs/") || path.Clean(target) != target {
		return errors.New("invalid run archive path")
	}
	var buffer bytes.Buffer
	tw := tar.NewWriter(&buffer)
	rel := strings.TrimPrefix(target, "/workspace/")
	parts := strings.Split(rel, "/")
	for n := 1; n < len(parts); n++ {
		if err := tw.WriteHeader(&tar.Header{Name: strings.Join(parts[:n], "/"), Typeflag: tar.TypeDir, Mode: 0700}); err != nil {
			return err
		}
	}
	if err := tw.WriteHeader(&tar.Header{Name: rel, Size: int64(len(data)), Mode: 0600}); err != nil {
		return err
	}
	if _, err := tw.Write(data); err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	res, err := c.request(ctx, "PUT", "/containers/"+url.PathEscape(name)+"/archive?path=%2Fworkspace", &buffer, "application/x-tar")
	if err != nil {
		return err
	}
	res.Body.Close()
	return nil
}

// exec starts without TTY; Docker frames stdout/stderr with an 8-byte header.
func (c *Client) exec(ctx context.Context, name string, argv, env []string, out io.Writer) (string, error) {
	var created struct {
		ID string `json:"Id"`
	}
	err := c.json(ctx, "POST", "/containers/"+url.PathEscape(name)+"/exec", map[string]any{"AttachStdout": true, "AttachStderr": true, "Tty": false, "Cmd": argv, "Env": env, "WorkingDir": "/workspace"}, &created)
	if err != nil {
		return "", err
	}
	if created.ID == "" {
		return "", errors.New("Docker returned an empty exec ID")
	}
	data := strings.NewReader(`{"Detach":false,"Tty":false}`)
	res, err := c.request(ctx, "POST", "/exec/"+created.ID+"/start", data, "application/json")
	if err != nil {
		return created.ID, err
	}
	defer res.Body.Close()
	for {
		var header [8]byte
		_, err = io.ReadFull(res.Body, header[:])
		if err == io.EOF {
			break
		}
		if err != nil {
			return created.ID, err
		}
		size := binary.BigEndian.Uint32(header[4:])
		if size > 32<<20 {
			return created.ID, errors.New("oversized Docker stream frame")
		}
		destination := out
		if header[0] != 1 {
			destination = io.Discard
		}
		if _, err = io.CopyN(destination, res.Body, int64(size)); err != nil {
			return created.ID, err
		}
	}
	var info struct {
		Running bool `json:"Running"`
		Exit    int  `json:"ExitCode"`
	}
	for {
		if err = c.json(ctx, "GET", "/exec/"+created.ID+"/json", nil, &info); err != nil {
			return created.ID, err
		}
		if !info.Running {
			if info.Exit != 0 {
				return created.ID, fmt.Errorf("worker process exited with code %d", info.Exit)
			}
			return created.ID, nil
		}
		select {
		case <-ctx.Done():
			return created.ID, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

type resultSink struct {
	pending     []byte
	result      worker.Result
	found       bool
	graph       func(worker.GraphRequest) error
	model       func(modelrpc.Request) error
	cancelModel func(string) error
}

func (s *resultSink) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		end := bytes.IndexByte(p, '\n')
		if end < 0 {
			s.pending = append(s.pending, p...)
			break
		}
		s.pending = append(s.pending, p[:end]...)
		if len(s.pending) > 32<<20 {
			return 0, errors.New("worker event too large")
		}
		// Most lines are model/tool journal events. Inspect their envelope once
		// without allocating fields that the dispatcher never consumes.
		var envelope struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(s.pending, &envelope)
		switch envelope.Type {
		case "model_request":
			var event modelrpc.RequestEvent
			if err := json.Unmarshal(s.pending, &event); err != nil {
				return 0, err
			}
			if s.model == nil {
				return 0, errors.New("worker requested an unavailable model bridge")
			}
			if err := s.model(event.Request); err != nil {
				return 0, err
			}
		case "model_cancel":
			var event modelrpc.CancelEvent
			if err := json.Unmarshal(s.pending, &event); err != nil {
				return 0, err
			}
			if s.cancelModel == nil {
				return 0, errors.New("model bridge is unavailable")
			}
			if err := s.cancelModel(event.RequestID); err != nil {
				return 0, err
			}
		case "graph_request":
			if len(s.pending) > worker.MaxGraphRPCBytes {
				return 0, errors.New("graph request exceeds 128 KiB")
			}
			var event worker.GraphRequestEvent
			if err := json.Unmarshal(s.pending, &event); err != nil {
				return 0, err
			}
			if s.graph == nil {
				return 0, errors.New("worker requested an unavailable graph bridge")
			}
			if err := s.graph(event.Request); err != nil {
				return 0, err
			}
		case "result":
			if s.found {
				return 0, errors.New("worker emitted multiple results")
			}
			var result worker.Result
			if err := json.Unmarshal(s.pending, &result); err != nil {
				return 0, err
			}
			s.result = result
			s.found = true
		}
		if cap(s.pending) > 64<<10 {
			s.pending = nil
		} else {
			s.pending = s.pending[:0]
		}
		p = p[end+1:]
	}
	if len(s.pending) > 32<<20 {
		return 0, errors.New("worker event too large")
	}
	return n, nil
}
func (c *Client) Run(ctx context.Context, w config.Worker, j worker.Job) (worker.Result, error) {
	if j.RunID == "" || strings.ContainsAny(j.RunID, "/\\.") {
		return worker.Result{}, errors.New("invalid run ID")
	}
	for _, ch := range j.RunID {
		if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '-' || ch == '_') {
			return worker.Result{}, errors.New("invalid run ID")
		}
	}
	p, err := configuredModel(w, j)
	if err != nil {
		return worker.Result{}, err
	}
	if p.Client != nil {
		defer p.Client.CloseIdleConnections()
	}
	env, err := modelWorkerEnv(w, p.Token)
	if err != nil {
		return worker.Result{}, err
	}
	name, err := c.ensure(ctx, j.Graph.Project.ID, p.Token)
	if err != nil {
		return worker.Result{}, err
	}
	// A shared network's hosts file belongs to its owner, not this worker.
	if w.Env["PWNMESH_CONNECTION_MODE"] == "proxy" && !strings.HasPrefix(c.Config.Network, "container:") {
		if err = c.ensureProxyHost(ctx, name, w.Env["PWNMESH_PROXY_URL"]); err != nil {
			return worker.Result{}, err
		}
	}
	if err = c.stageInputs(ctx, name, j); err != nil {
		return worker.Result{}, err
	}
	target := "/workspace/.pwnmesh/runs/" + j.RunID + "/job.json"
	raw, err := json.Marshal(j)
	if err != nil {
		return worker.Result{}, err
	}
	if jsonContainsModelCredential(raw, p.Token) {
		return worker.Result{}, errors.New("model credential must not be included in a worker job")
	}
	if err = c.archive(ctx, name, target, raw); err != nil {
		return worker.Result{}, err
	}
	settings, err := json.Marshal(publicModelSettings(p))
	if err != nil || jsonContainsModelCredential(settings, p.Token) {
		return worker.Result{}, errors.New("invalid public model settings")
	}
	if err = c.archive(ctx, name, path.Join(path.Dir(target), "model-settings.json"), settings); err != nil {
		return worker.Result{}, err
	}
	launch := make([]byte, 16)
	if _, err = rand.Read(launch); err != nil {
		return worker.Result{}, err
	}
	launchToken := hex.EncodeToString(launch)
	if err = c.archive(ctx, name, path.Join(path.Dir(target), "launch-token"), []byte(launchToken)); err != nil {
		return worker.Result{}, err
	}
	env = append(env, "PWNMESH_LAUNCH_TOKEN="+launchToken)
	sort.Strings(env)
	bridgeCtx, abortBridge := context.WithCancelCause(ctx)
	defer abortBridge(nil)
	bridge := newModelBridge(bridgeCtx, p, j, c.modelDelivery(name, path.Dir(target)), abortBridge)
	defer bridge.close()
	sink := &resultSink{graph: c.graphBridge(bridgeCtx, name, path.Dir(target), j), model: bridge.start, cancelModel: bridge.stop}
	_, err = c.exec(bridgeCtx, name, []string{"/usr/local/bin/pwnmesh", "worker", "--job", target}, env, sink)
	if bridgeCtx.Err() != nil && ctx.Err() == nil {
		err = context.Cause(bridgeCtx)
	}
	if err == nil && ctx.Err() == nil {
		if len(sink.pending) > 0 {
			_, err = sink.Write([]byte{'\n'})
		}
		if err == nil && !sink.found {
			err = errors.New("worker exited without a result")
		}
	}
	if err != nil || ctx.Err() != nil {
		if ctx.Err() != nil && !errors.Is(context.Cause(ctx), worker.ErrInterrupted) {
			c.cancel(name, path.Dir(target))
		} else if stopErr := c.interrupt(name, path.Dir(target)); stopErr != nil {
			return worker.Result{}, errors.Join(err, fmt.Errorf("cannot confirm execution interruption: %w", stopErr))
		}
		if ctx.Err() != nil {
			return worker.Result{}, ctx.Err()
		}
		return worker.Result{}, err
	}
	return sink.result, nil
}

func (c *Client) interrupt(name, runDir string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	_, err := c.exec(ctx, name, []string{"/usr/local/bin/pwnmesh", "worker", "--interrupt", runDir}, nil, io.Discard)
	return err
}
func (c *Client) cancel(name, runDir string) {
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	c.exec(ctx, name, []string{"/usr/local/bin/pwnmesh", "worker", "--cancel", runDir}, nil, io.Discard)
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return
	case <-timer.C:
	}
	c.exec(ctx, name, []string{"/usr/local/bin/pwnmesh", "worker", "--cancel", runDir, "--force"}, nil, io.Discard)
}
func (c *Client) Cleanup(ctx context.Context, id, state string) error {
	unlock := c.lock(id)
	defer unlock()
	var info struct {
		ID     string `json:"Id"`
		Config struct {
			Labels map[string]string `json:"Labels"`
		} `json:"Config"`
	}
	err := c.json(ctx, "GET", "/containers/"+url.PathEscape(c.name(id))+"/json", nil, &info)
	if status(err, 404) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Config.Labels["pwnmesh.namespace"] != c.Config.Namespace || info.Config.Labels["pwnmesh.project"] != id {
		return errors.New("refusing to clean up a container belonging to a different project or dispatcher namespace")
	}
	if info.ID == "" {
		return errors.New("Docker returned an empty container ID during cleanup")
	}
	// Names can be reassigned after inspection. Target the verified, immutable
	// container ID so a replacement with the same name is never stopped/removed.
	route := "/containers/" + url.PathEscape(info.ID)
	if state == "deleted" || (state == "completed" && c.Config.CompletedAction == "remove") {
		err = c.json(ctx, "DELETE", route+"?force=true", nil, nil)
	} else {
		err = c.json(ctx, "POST", route+"/stop?t=1", nil, nil)
	}
	if status(err, 404) || status(err, 304) {
		return nil
	}
	return err
}
func (c *Client) Projects(ctx context.Context) ([]string, error) {
	filters, _ := json.Marshal(map[string][]string{"label": {"pwnmesh.namespace=" + c.Config.Namespace}})
	var containers []struct {
		Labels map[string]string `json:"Labels"`
	}
	if err := c.json(ctx, "GET", "/containers/json?all=true&filters="+url.QueryEscape(string(filters)), nil, &containers); err != nil {
		return nil, err
	}
	ids := []string{}
	for _, item := range containers {
		if id := item.Labels["pwnmesh.project"]; id != "" {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids, nil
}
