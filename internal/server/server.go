package server

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	b "pwnmesh/internal/board"
	"pwnmesh/web"
)

type Server struct{ Store *b.Store }
type request struct {
	fields map[string]any
	err    error
}
type action func(*b.Tx, *request, *http.Request) (int, any, error)

func decodeStrictFields(q *request, v any) error {
	raw, err := json.Marshal(q.fields)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	return decoder.Decode(v)
}

func New(store *b.Store) http.Handler {
	return NewWithModelSettings(store, "")
}

func NewWithModelSettings(store *b.Store, modelSettingsPath string) http.Handler {
	s := &Server{store}
	m := http.NewServeMux()
	registerModelSettings(m, modelSettingsPath)
	m.HandleFunc("GET /static", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/static/", http.StatusTemporaryRedirect)
	})
	m.HandleFunc("GET /static/", func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/static/")
		data, err := fs.ReadFile(web.Files, name)
		if err != nil {
			writeError(w, r, b.Err(404, "Not Found"))
			return
		}
		http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(data))
	})
	m.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		data, err := fs.ReadFile(web.Files, "index.html")
		if err != nil {
			http.Error(w, "Web unavailable", 500)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(data)
	})
	m.HandleFunc("GET /health", s.readOnly(s.health))
	m.HandleFunc("GET /settings", s.readOnly(s.settings))
	m.HandleFunc("GET /projects", s.readOnly(s.projects))
	m.HandleFunc("GET /projects/{pid}", s.readOnly(s.project))
	m.HandleFunc("GET /projects/{pid}/export", s.readOnly(s.export))
	for pattern, fn := range map[string]action{
		"PUT /settings":  s.settings,
		"POST /projects": s.projects, "DELETE /projects/{pid}": s.project,
		"PUT /projects/{pid}/title": s.title, "PUT /projects/{pid}/status": s.status,
		"POST /projects/{pid}/hints": s.hint, "POST /projects/{pid}/reopen": s.reopen,
		"POST /projects/{pid}/restart":   s.restart,
		"POST /projects/{pid}/terminate": s.terminate,
	} {
		m.HandleFunc(pattern, s.wrap(fn))
	}
	for _, op := range []string{"claim", "heartbeat", "release"} {
		m.HandleFunc("POST /projects/{pid}/reason/"+op, func(w http.ResponseWriter, r *http.Request) {
			r.SetPathValue("op", op)
			s.wrap(s.reason)(w, r)
		})
		m.HandleFunc("POST /projects/{pid}/curate/"+op, func(w http.ResponseWriter, r *http.Request) {
			r.SetPathValue("op", op)
			s.wrap(s.curate)(w, r)
		})
	}
	for _, op := range []string{"heartbeat", "release"} {
		m.HandleFunc("POST /projects/{pid}/intents/{iid}/"+op, func(w http.ResponseWriter, r *http.Request) {
			r.SetPathValue("op", op)
			s.wrap(s.intentAction)(w, r)
		})
	}
	s.registerStateRoutes(m)
	s.registerExecutionRoutes(m)
	s.registerObservationRoutes(m)
	s.registerUIRoutes(m)
	s.registerDeploymentRoutes(m)
	s.registerRoundRoutes(m)
	s.registerInputRoutes(m)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !allowBrowserMutation(r) {
			writeError(w, r, b.Err(http.StatusForbidden, "Cross-origin mutations are not allowed"))
			return
		}
		_, pattern := m.Handler(r)
		if pattern == "" {
			// Normalize trailing slashes and return JSON routing errors.
			if len(r.URL.Path) > 1 && strings.HasSuffix(r.URL.Path, "/") {
				probe := r.Clone(r.Context())
				probe.URL.Path = strings.TrimRight(r.URL.Path, "/")
				probe.URL.RawPath = ""
				if _, alternate := m.Handler(probe); alternate != "" {
					http.Redirect(w, r, probe.URL.RequestURI(), http.StatusTemporaryRedirect)
					return
				}
			}
			allowed := []string{}
			for _, method := range []string{"GET", "HEAD", "POST", "PUT", "DELETE"} {
				probe := r.Clone(r.Context())
				probe.Method = method
				if _, other := m.Handler(probe); other != "" {
					allowed = append(allowed, method)
				}
			}
			if len(allowed) > 0 {
				w.Header().Set("Allow", strings.Join(allowed, ", "))
				writeError(w, r, b.Err(405, "Method Not Allowed"))
			} else {
				writeError(w, r, b.Err(404, "Not Found"))
			}
			return
		}
		m.ServeHTTP(w, r)
	})
}

// The local control API accepts ordinary API clients without browser headers.
// Browsers must originate mutations from this server, including simple POSTs
// with text/plain bodies, which otherwise bypass CORS preflight protection.
func allowBrowserMutation(r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
		return true
	}
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") &&
		u.User == nil && u.Path == "" && u.RawQuery == "" && u.Fragment == "" &&
		strings.EqualFold(u.Host, r.Host)
}

func (s *Server) wrap(fn action) http.HandlerFunc {
	return s.wrapAction(fn, false)
}

// Observational endpoints keep expensive history, projection and blob reads
// outside the writer reservation. Scheduling and all lease mutations still
// expire leases under the original immediate transaction.
func (s *Server) readOnly(fn action) http.HandlerFunc {
	return s.wrapAction(fn, true)
}

func (s *Server) wrapAction(fn action, readOnly bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		in := &request{fields: map[string]any{}}
		if r.Method == "POST" || r.Method == "PUT" {
			dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20))
			dec.UseNumber()
			if err := dec.Decode(&in.fields); err != nil || in.fields == nil {
				writeError(w, r, &b.APIError{Status: 422, Detail: "Expected a JSON object"})
				return
			}
			var extra any
			if err := dec.Decode(&extra); err != io.EOF {
				writeError(w, r, &b.APIError{Status: 422, Detail: "Expected one JSON object"})
				return
			}
		}
		status := 200
		var body any
		transaction := s.Store.Do
		if readOnly {
			transaction = s.Store.View
		}
		err := transaction(r.Context(), func(t *b.Tx) error {
			project := r.PathValue("pid")
			if project != "" {
				if err := t.RequireCurrentProject(project); err != nil {
					return err
				}
				if !readOnly {
					if err := t.ExpireProject(project); err != nil {
						return err
					}
				}
			} else if !readOnly {
				if err := t.ExpireProject(""); err != nil {
					return err
				}
			}
			var err error
			status, body, err = fn(t, in, r)
			if in.err != nil {
				return in.err
			}
			return err
		})
		if err != nil {
			writeError(w, r, err)
			return
		}
		if txt, ok := body.(plain); ok {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(status)
			io.WriteString(w, string(txt))
			return
		}
		if status == 204 {
			w.WriteHeader(status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if err := json.NewEncoder(w).Encode(body); err != nil {
			slog.Warn("response write failed", "error", err, "method", r.Method, "path", r.URL.Path, "context_error", r.Context().Err())
		}
	}
}

func writeError(w http.ResponseWriter, r *http.Request, err error) {
	var ae *b.APIError
	if !errors.As(err, &ae) {
		slog.Error("server request failed", "error", err, "method", r.Method, "path", r.URL.Path, "context_error", r.Context().Err())
		ae = &b.APIError{Status: 500, Detail: "Internal Server Error"}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(ae.Status)
	json.NewEncoder(w).Encode(map[string]any{"detail": ae.Detail})
}
func (r *request) invalid(key, msg string) {
	if r.err == nil {
		r.err = invalidField(key, msg)
	}
}
func invalidField(key, msg string) error {
	return &b.APIError{Status: 422, Detail: []any{map[string]any{"loc": []string{"body", key}, "msg": msg, "type": "value_error"}}}
}
func (r *request) text(key string) string {
	v, ok := r.fields[key].(string)
	v = strings.TrimSpace(v)
	if !ok || v == "" {
		r.invalid(key, "must be a non-empty string")
	}
	return v
}
func (r *request) integer(key string) int {
	number, ok := r.fields[key].(json.Number)
	n, err := strconv.Atoi(string(number))
	if !ok || err != nil || n < 5 {
		r.invalid(key, "must be an integer greater than or equal to 5")
	}
	return n
}
func (r *request) validateBootstrap() {
	if value, exists := r.fields["bootstrap_enabled"]; exists {
		if enabled, ok := value.(bool); !ok || enabled {
			r.invalid("bootstrap_enabled", "legacy bootstrap is unsupported; omit this field or set false")
		}
	}
}

func (r *request) validateOrchestrationVersion() {
	value, exists := r.fields["orchestration_version"]
	if !exists {
		return
	}
	// Refactor has one protocol. Never coerce an explicit old or malformed
	// value into the current evidence-writing authority.
	number, ok := value.(json.Number)
	if !ok || number != "1" {
		r.invalid("orchestration_version", "must be the integer 1; legacy execution is unsupported")
	}
}

// Headers opt a dispatcher request into the board's transactional lease fence.
func guard(t *b.Tx, g b.Graph, r *http.Request) error {
	return t.CheckExecution(g, decisionFence(r))
}

func (s *Server) health(t *b.Tx, _ *request, _ *http.Request) (int, any, error) {
	_, err := t.Settings()
	return 200, map[string]string{"status": "ok"}, err
}
func (s *Server) settings(t *b.Tx, q *request, r *http.Request) (int, any, error) {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		v, err := t.Settings()
		return 200, v, err
	}
	v := b.Settings{IntentTimeout: q.integer("intent_timeout"), ReasonTimeout: q.integer("reason_timeout")}
	if q.err != nil {
		return 0, nil, q.err
	}
	err := t.UpdateSettings(v)
	return 200, v, err
}
func (s *Server) projects(t *b.Tx, q *request, r *http.Request) (int, any, error) {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		out, err := t.ObservedProjectSummaries()
		return 200, out, err
	}
	input := b.ProjectInput{Title: q.text("title"), Origin: q.text("origin"), Goal: q.text("goal")}
	q.validateBootstrap()
	q.validateOrchestrationVersion()
	if _, exists := q.fields["scenario"]; exists {
		input.Scenario = q.text("scenario")
		if !b.ValidScenario(input.Scenario) {
			q.invalid("scenario", "must be ctf, pentest or audit")
		}
	}
	if q.err != nil {
		return 0, nil, q.err
	}
	if value, exists := q.fields["start_paused"]; exists {
		paused, ok := value.(bool)
		if !ok {
			return 0, nil, b.Err(422, "start_paused must be a boolean")
		}
		input.StartPaused = paused
	}
	if raw := q.fields["hints"]; raw != nil {
		list, ok := raw.([]any)
		if !ok {
			return 0, nil, b.Err(422, "hints must be an array")
		}
		for _, item := range list {
			fields, ok := item.(map[string]any)
			if !ok {
				return 0, nil, b.Err(422, "hint must be an object")
			}
			h := &request{fields: fields}
			hint := b.HintInput{Content: h.text("content"), Creator: h.text("creator")}
			if h.err != nil {
				return 0, nil, h.err
			}
			input.Hints = append(input.Hints, hint)
		}
	}
	graph, err := t.CreateProject(input)
	return http.StatusCreated, graph, err
}

func (s *Server) project(t *b.Tx, _ *request, r *http.Request) (int, any, error) {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		graph, err := t.Load(r.PathValue("pid"))
		return http.StatusOK, graph, err
	}
	return http.StatusNoContent, nil, t.DeleteProject(r.PathValue("pid"))
}

func (s *Server) title(t *b.Tx, q *request, r *http.Request) (int, any, error) {
	title := q.text("title")
	if q.err != nil {
		return 0, nil, q.err
	}
	project, err := t.SetProjectTitle(r.PathValue("pid"), title)
	return http.StatusOK, project, err
}

func (s *Server) status(t *b.Tx, q *request, r *http.Request) (int, any, error) {
	status, _ := q.fields["status"].(string)
	if status != "active" && status != "stopped" {
		q.invalid("status", "must be active or stopped")
	}
	if q.err != nil {
		return 0, nil, q.err
	}
	expected, err := managementGeneration(q, "status", "status")
	if err != nil {
		return 0, nil, err
	}
	project, err := t.SetProjectStatus(r.PathValue("pid"), status, expected)
	return http.StatusOK, project, err
}

func (s *Server) reason(t *b.Tx, q *request, r *http.Request) (int, any, error) {
	input := leaseOperation(q, r)
	if q.err != nil {
		return 0, nil, q.err
	}
	project, err := t.ReasonLease(r.PathValue("pid"), input)
	return http.StatusOK, project, err
}

func leaseOperation(q *request, r *http.Request) b.LeaseOperation {
	input := b.LeaseOperation{Op: r.PathValue("op"), Worker: q.text("worker"), Fence: decisionFence(r)}
	if input.Op == "claim" {
		input.Trigger = q.text("trigger")
	}
	if input.Op == "heartbeat" {
		if _, present := q.fields["expected_version"]; present {
			expected := q.text("expected_version")
			if _, err := hex.DecodeString(expected); len(expected) != 64 || err != nil {
				q.invalid("expected_version", "must be a 64-character hexadecimal state version")
			}
			input.ExpectedVersion = &expected
		}
	}
	return input
}

func (s *Server) hint(t *b.Tx, q *request, r *http.Request) (int, any, error) {
	input := b.HintInput{Content: q.text("content"), Creator: q.text("creator")}
	if q.err != nil {
		return 0, nil, q.err
	}
	hint, err := t.AddProjectHint(r.PathValue("pid"), input, r.Header.Get("X-PwnMesh-Run"))
	return http.StatusCreated, hint, err
}

func (s *Server) intentAction(t *b.Tx, q *request, r *http.Request) (int, any, error) {
	input := b.LeaseOperation{Op: r.PathValue("op"), Worker: q.text("worker"), Fence: decisionFence(r)}
	if q.err != nil {
		return 0, nil, q.err
	}
	intent, err := t.IntentLease(r.PathValue("pid"), r.PathValue("iid"), input)
	return http.StatusOK, intent, err
}

func (s *Server) reopen(t *b.Tx, q *request, r *http.Request) (int, any, error) {
	description, creator := q.text("description"), q.text("creator")
	if q.err != nil {
		return 0, nil, q.err
	}
	result, err := t.ReopenProject(r.PathValue("pid"), description, creator, r.Header.Get("X-PwnMesh-Run"))
	return http.StatusOK, result, err
}

type plain string

func (s *Server) export(t *b.Tx, _ *request, r *http.Request) (int, any, error) {
	format := r.URL.Query().Get("format")
	if format == "" {
		format = "yaml"
	}
	if format != "yaml" && format != "timeline" {
		return 0, nil, b.Err(400, "Supported formats: yaml, timeline")
	}
	if format == "timeline" {
		text, err := t.ExportTimeline(r.PathValue("pid"))
		return 200, plain(text), err
	}
	g, err := t.Load(r.PathValue("pid"))
	if err != nil {
		return 0, nil, err
	}
	text, err := b.Export(g, format)
	return 200, plain(text), err
}
