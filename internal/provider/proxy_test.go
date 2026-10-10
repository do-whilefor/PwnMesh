package provider

import (
	"net/http"
	"net/url"
	"testing"
)

func TestProxyURLMapsOnlyHostLoopbackInContainers(t *testing.T) {
	for _, tc := range []struct {
		raw       string
		container bool
		want      string
	}{
		{"http://127.0.0.1:7897", true, "http://host.docker.internal:7897"},
		{"http://localhost:7897", true, "http://host.docker.internal:7897"},
		{"socks5h://[::1]:7897", true, "socks5h://host.docker.internal:7897"},
		{"https://proxy.example:8443", true, "https://proxy.example:8443"},
		{"http://proxy.example:1", false, "http://proxy.example:1"},
		{"socks5://proxy.example:65535", false, "socks5://proxy.example:65535"},
		{"socks5h://proxy.example", false, "socks5h://proxy.example"},
		{"http://127.0.0.1:7897", false, "http://127.0.0.1:7897"},
	} {
		got, err := ProxyURL(tc.raw, tc.container)
		if err != nil || got != tc.want {
			t.Errorf("ProxyURL(%s)=%s, %v", tc.raw, got, err)
		}
	}
	for _, raw := range []string{"", "ftp://proxy:7897", "http://user:secret@proxy:7897", "http://proxy:7897/path", "http://proxy:7897?token=secret", "http://proxy:0", "https://proxy:65536", "socks5://proxy:99999999999999999999999"} {
		if _, err := ProxyURL(raw, true); err == nil {
			t.Errorf("accepted invalid proxy %q", raw)
		}
	}
}

func TestExplicitConnectionModeControlsModelTransport(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://ambient.invalid:7897")
	t.Setenv("HTTPS_PROXY", "http://ambient.invalid:7897")
	getenv := func(mode string) func(string) string {
		return func(key string) string {
			return map[string]string{"ANTHROPIC_AUTH_TOKEN": "fixture", "PWNMESH_CONNECTION_MODE": mode, "PWNMESH_PROXY_URL": "http://localhost:7897"}[key]
		}
	}
	direct, err := FromEnvironment(getenv("direct"), "")
	if err != nil {
		t.Fatal(err)
	}
	if direct.Client.Transport.(*http.Transport).Proxy != nil {
		t.Fatal("direct connection inherited ambient proxy")
	}
	proxy, err := FromEnvironment(getenv("proxy"), "")
	if err != nil {
		t.Fatal(err)
	}
	target, _ := url.Parse("https://models.invalid/v1/messages")
	route, err := proxy.Client.Transport.(*http.Transport).Proxy(&http.Request{URL: target})
	want, _ := ProxyURL("http://localhost:7897", InContainer())
	if err != nil || route.String() != want {
		t.Fatalf("wrong model proxy: %v %v", route, err)
	}
}
