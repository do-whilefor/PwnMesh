package server

import (
	"net/http"
	"testing"

	"pwnmesh/internal/board"
)

func TestProjectExecutionPagingHasOneResponseShapeAndValidatesBoundaries(t *testing.T) {
	f := newExecutionProtocolFixture(t)
	f.register("reason")
	var defaults board.ExecutionViewPage
	f.request("GET", f.base()+"/executions", nil, false, http.StatusOK, &defaults)
	var page board.ExecutionViewPage
	f.request("GET", f.base()+"/executions?limit=1&cursor=0&through=0", nil, false, http.StatusOK, &page)
	if len(defaults.Items) != 1 || len(page.Items) != 1 || page.Items[0].ID != defaults.Items[0].ID || page.Items[0].Generation != defaults.Items[0].Generation || page.NextCursor != 0 || page.Through == 0 || page.Through != defaults.Through {
		t.Fatalf("default paging differs from explicit page: %+v %+v", defaults, page)
	}
	f.request("GET", f.base()+"/executions?limit=1&cursor=3&through=2", nil, false, http.StatusUnprocessableEntity, nil)
	f.request("GET", "/projects/missing/executions?limit=20", nil, false, http.StatusNotFound, nil)
}

func TestProjectIdentityTracksRestartsWithoutGraphPayload(t *testing.T) {
	f := newExecutionProtocolFixture(t)
	var identity map[string]any
	f.request("GET", f.base()+"/identity", nil, false, http.StatusOK, &identity)
	if len(identity) != 2 || identity["id"] != f.project || identity["generation"] != float64(0) {
		t.Fatalf("unexpected initial identity: %+v", identity)
	}
	f.request("POST", f.base()+"/restart", map[string]int{"expected_generation": 0}, false, http.StatusOK, nil)
	f.request("GET", f.base()+"/identity", nil, false, http.StatusOK, &identity)
	if len(identity) != 2 || identity["id"] != f.project || identity["generation"] != float64(1) {
		t.Fatalf("identity missed restart: %+v", identity)
	}
	f.request("GET", "/projects/missing/identity", nil, false, http.StatusNotFound, nil)
}

func TestExecutionChangeEndpointRequiresGenerationAndReportsRestartConflict(t *testing.T) {
	f := newExecutionProtocolFixture(t)
	f.register("reason")
	var page board.ExecutionChangePage
	f.request("GET", f.base()+"/executions/changes?generation=0", nil, false, http.StatusOK, &page)
	if len(page.Items) != 1 || page.Through == 0 || page.Generation != 0 {
		t.Fatalf("missing execution change projection: %+v", page)
	}
	f.request("GET", f.base()+"/executions/changes", nil, false, http.StatusUnprocessableEntity, nil)
	f.request("POST", f.base()+"/restart", map[string]int{"expected_generation": 0}, false, http.StatusOK, nil)
	f.request("GET", f.base()+"/executions/changes?generation=0", nil, false, http.StatusConflict, nil)
	f.request("GET", f.base()+"/executions/changes?generation=1", nil, false, http.StatusOK, nil)
}

func TestProjectExecutionPagingRejectsInvalidNumbers(t *testing.T) {
	f := newExecutionProtocolFixture(t)
	for _, view := range []struct {
		name   string
		path   string
		fields []string
	}{
		{"page", "/executions?", []string{"limit", "cursor", "through"}},
		{"change", "/executions/changes?", []string{"generation", "after", "cursor", "through", "limit"}},
	} {
		for _, field := range view.fields {
			values := []string{"", "-1", "invalid", "9223372036854775808"}
			if field == "limit" {
				values = append(values, "0", "101")
			}
			for _, value := range values {
				t.Run(view.name+"/"+field+"/"+value, func(t *testing.T) {
					fixture := *f
					fixture.t = t
					query := field + "=" + value
					if view.name == "change" && field != "generation" {
						query += "&generation=0"
					}
					var response struct {
						Detail string `json:"detail"`
					}
					fixture.request("GET", f.base()+view.path+query, nil, false, http.StatusUnprocessableEntity, &response)
					if want := "invalid execution " + view.name + " " + field; response.Detail != want {
						t.Fatalf("error detail = %q, want %q", response.Detail, want)
					}
				})
			}
		}
	}
}

func TestRetiredHTTPRoutesAreUnavailable(t *testing.T) {
	f := newExecutionProtocolFixture(t)
	for _, path := range []string{"/ui/overview", "/executions?namespace=protocol-test", f.base() + "/state/changes?after=0&through=1"} {
		f.request("GET", path, nil, false, http.StatusNotFound, nil)
	}
	f.request("POST", f.base()+"/executions", map[string]any{}, false, http.StatusMethodNotAllowed, nil)
}
