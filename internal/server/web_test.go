package server

import (
	"io/fs"
	"mime"
	"net/http"
	"net/http/httptest"
	"path"
	"regexp"
	"strings"
	"testing"

	"pwnmesh/web"
)

func TestWorkbenchServesDocumentWithSameOriginPolicy(t *testing.T) {
	handler := New(nil)
	for _, endpoint := range []string{"/", "/static/index.html"} {
		t.Run(endpoint, func(t *testing.T) {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, endpoint, nil))
			if response.Code != http.StatusOK {
				t.Fatalf("workspace returned HTTP %d", response.Code)
			}
			if !strings.HasPrefix(response.Header().Get("Content-Type"), "text/html") {
				t.Fatalf("workspace content type = %q", response.Header().Get("Content-Type"))
			}
			html := response.Body.String()
			for _, want := range []string{"<title>PwnMesh · 安全探索工作台</title>", `src="/static/brand.png"`, `id="main"`, `id="project-list"`, `id="create-form"`, `id="project-dialog"`, `id="i-target"`} {
				if !strings.Contains(html, want) {
					t.Errorf("PwnMesh workspace is missing %q", want)
				}
			}
			if regexp.MustCompile(`\b(?:Pwn|X-Loom)\b`).MatchString(html) {
				t.Error("workspace still displays the previous brand")
			}
			if strings.Contains(html, "legacy.html") || strings.Contains(html, "经典管理界面") {
				t.Fatal("workspace still links to the removed classic interface")
			}
			policy := regexp.MustCompile(`<meta\s+http-equiv="Content-Security-Policy"\s+content="([^"]+)"`).FindStringSubmatch(html)
			if len(policy) != 2 {
				t.Fatal("workspace response is missing its content security policy")
			}
			directives := map[string]string{}
			for _, directive := range strings.Split(policy[1], ";") {
				parts := strings.Fields(directive)
				if len(parts) > 0 {
					directives[parts[0]] = strings.Join(parts[1:], " ")
				}
			}
			for directive, want := range map[string]string{
				"default-src": "'self'", "script-src": "'self'", "connect-src": "'self'",
				"object-src": "'none'", "base-uri": "'none'", "form-action": "'self'",
			} {
				if got := directives[directive]; got != want {
					t.Errorf("CSP %s = %q, want %q", directive, got, want)
				}
			}
			for _, script := range regexp.MustCompile(`(?s)<script\b([^>]*)>(.*?)</script>`).FindAllStringSubmatch(html, -1) {
				if strings.TrimSpace(script[2]) != "" || !strings.Contains(script[1], `src="/static/`) {
					t.Error("workspace must load scripts from its embedded same-origin assets")
				}
			}
		})
	}
}

func TestWorkbenchLoadsEmbeddedCanvasAssets(t *testing.T) {
	handler := New(nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	html := response.Body.String()
	loaded := map[string]bool{}
	for _, tag := range regexp.MustCompile(`<(?:script|link|img)\b[^>]*>`).FindAllString(html, -1) {
		for _, reference := range regexp.MustCompile(`(?:src|href)="([^"]+)"`).FindAllStringSubmatch(tag, -1) {
			endpoint := reference[1]
			if !strings.HasPrefix(endpoint, "/static/") {
				t.Errorf("workspace asset must use /static/: %s", endpoint)
				continue
			}
			loaded[endpoint] = true
			for _, method := range []string{http.MethodGet, http.MethodHead} {
				t.Run(method+endpoint, func(t *testing.T) {
					asset := httptest.NewRecorder()
					handler.ServeHTTP(asset, httptest.NewRequest(method, endpoint, nil))
					if asset.Code != http.StatusOK {
						t.Fatalf("asset returned HTTP %d", asset.Code)
					}
					if method == http.MethodGet && asset.Body.Len() == 0 {
						t.Error("embedded asset is empty")
					}
					if method == http.MethodHead && asset.Body.Len() != 0 {
						t.Error("HEAD response contains an asset body")
					}
					mediaType, _, err := mime.ParseMediaType(asset.Header().Get("Content-Type"))
					want := map[string]string{".js": "javascript", ".css": "text/css", ".svg": "image/svg+xml", ".png": "image/png", ".woff2": "font/woff2"}[path.Ext(endpoint)]
					if err != nil || want == "" || !strings.Contains(mediaType, want) {
						t.Errorf("asset Content-Type = %q for %s", asset.Header().Get("Content-Type"), endpoint)
					}
				})
			}
		}
	}
	for _, name := range []string{
		"api.js", "data.js", "graph-data.js", "canvas.js", "layout.js", "routing.js",
		"graph-view.js", "graph.js", "app.js", "style.css", "graph.css", "brand.png", "models.js", "llm.js", "llm.css", "shell.css", "blackboard.css", "forms.css", "inspector.css", "lucide.min.js",
	} {
		if !loaded["/static/"+name] {
			t.Errorf("workspace does not load %s", name)
		}
	}
	for endpoint := range loaded {
		if strings.Contains(endpoint, "/vendor/") || strings.HasSuffix(endpoint, "/graph-demo.js") || strings.HasSuffix(endpoint, "/workbench.css") || strings.HasSuffix(endpoint, "/models.css") {
			t.Errorf("workspace loads a replaced renderer or demo data source: %s", endpoint)
		}
	}
}

func TestWorkbenchEmbedsNestedVisualAssets(t *testing.T) {
	handler := New(nil)
	err := fs.WalkDir(web.Files, ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/static/"+name, nil))
		if response.Code != http.StatusOK || response.Body.Len() == 0 {
			t.Errorf("embedded file %s: HTTP %d, bytes %d", name, response.Code, response.Body.Len())
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestWorkbenchLogoIdentifiesPwnMesh(t *testing.T) {
	handler := New(nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	html := response.Body.String()
	if !regexp.MustCompile(`<a\b[^>]*aria-label="PwnMesh 工作台"[^>]*><span\b[^>]*><img\b[^>]*src="/static/brand.png"[^>]*alt="PwnMesh"`).MatchString(html) {
		t.Fatal("visible logo must identify PwnMesh for assistive technology")
	}
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/static/brand.png", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("logo returned HTTP %d", response.Code)
	}
	if !strings.HasPrefix(response.Header().Get("Content-Type"), "image/png") || response.Body.Len() == 0 {
		t.Fatal("visible logo must serve its embedded PNG")
	}
}

func TestClassicInterfaceIsUnavailable(t *testing.T) {
	handler := New(nil)
	for _, path := range []string{
		"/legacy.html", "/static/legacy.html", "/static/favicon.svg",
		"/static/vendor/alpine.min.js", "/static/vendor/tailwindcss.js",
		"/static/vendor/cola.min.js", "/static/vendor/cytoscape-cola.js",
		"/static/vendor/klay.js", "/static/vendor/cytoscape-klay.js",
		"/static/vendor/elk.bundled.js", "/static/vendor/cytoscape-elk.js",
		"/static/vendor/cytoscape.min.js", "/static/vendor/dagre.min.js",
		"/static/vendor/cytoscape-dagre.js", "/static/pwnmesh.svg",
		"/static/workbench.css", "/static/models.css",
		"/static/mark.svg", "/static/provider-deepseek.svg", "/static/provider-glm.svg",
		"/static/provider-kimi.svg", "/static/provider-sources.json", "/static/LICENSE-lobe-icons.txt",
	} {
		t.Run(path, func(t *testing.T) {
			for _, method := range []string{http.MethodGet, http.MethodHead} {
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, httptest.NewRequest(method, path, nil))
				if response.Code != http.StatusNotFound {
					t.Errorf("%s %s: HTTP %d, want 404", method, path, response.Code)
				}
			}
		})
	}
}
