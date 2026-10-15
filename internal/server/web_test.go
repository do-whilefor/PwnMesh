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
			for _, want := range []string{"<title>PwnMesh · 安全探索工作台</title>", `id="app"`, `type="module"`} {
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
			if strings.Contains(html, "/src/") || strings.Contains(html, ".ts\"") {
				t.Fatal("workspace must serve compiled assets, not frontend source")
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

func TestWorkbenchLoadsEmbeddedBuildAssets(t *testing.T) {
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
	scripts, styles := 0, 0
	for endpoint := range loaded {
		switch path.Ext(endpoint) {
		case ".js":
			scripts++
		case ".css":
			styles++
		}
	}
	if scripts == 0 || styles == 0 {
		t.Fatalf("workspace must load its compiled application and styles: scripts=%d styles=%d", scripts, styles)
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
		if strings.HasSuffix(name, ".ts") || strings.HasSuffix(name, ".vue") || strings.HasSuffix(name, ".map") || strings.Contains(name, "node_modules/") {
			t.Errorf("frontend source or dependencies must not be embedded: %s", name)
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

func TestWorkbenchServesBrandAsset(t *testing.T) {
	handler := New(nil)
	response := httptest.NewRecorder()
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
		"/static/app.js", "/static/api.js", "/static/data.js", "/static/service.js", "/static/src/main.ts", "/src/main.ts",
		"/legacy.html", "/static/legacy.html", "/static/favicon.svg",
		"/static/vendor/alpine.min.js", "/static/vendor/tailwindcss.js",
		"/static/vendor/cola.min.js", "/static/vendor/cytoscape-cola.js",
		"/static/vendor/klay.js", "/static/vendor/cytoscape-klay.js",
		"/static/vendor/elk.bundled.js", "/static/vendor/cytoscape-elk.js",
		"/static/vendor/cytoscape.min.js", "/static/vendor/dagre.min.js",
		"/static/vendor/cytoscape-dagre.js", "/static/pwnmesh.svg",
		"/static/workbench.css", "/static/models.css", "/static/graph-adapter.js",
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
