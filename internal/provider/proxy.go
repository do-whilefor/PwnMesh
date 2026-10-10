package provider

import (
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
)

// ProxyURL maps host-local proxies to the Docker host only inside containers.
// Workers always pass true because their network namespace is not the host's.
func ProxyURL(raw string, container bool) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "socks5" && u.Scheme != "socks5h") || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("proxy_url must be an HTTP(S) or SOCKS5 URL without credentials, path or query")
	}
	if container && (strings.EqualFold(u.Hostname(), "localhost") || net.ParseIP(u.Hostname()).IsLoopback()) {
		host := "host.docker.internal"
		if u.Port() != "" {
			host = net.JoinHostPort(host, u.Port())
		}
		u.Host = host
	}
	return strings.TrimRight(u.String(), "/"), nil
}

func InContainer() bool {
	_, err := os.Stat("/.dockerenv")
	return err == nil || os.Getenv("PWNMESH_IN_CONTAINER") == "1"
}

func connectionClient(mode, proxy string) (*http.Client, error) {
	if mode == "" {
		return nil, nil // Preserve explicitly configured legacy transports.
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	switch mode {
	case "direct":
	case "proxy":
		raw, err := ProxyURL(proxy, InContainer())
		if err != nil {
			return nil, err
		}
		u, _ := url.Parse(raw)
		transport.Proxy = http.ProxyURL(u)
	default:
		return nil, errors.New("connection_mode must be direct or proxy")
	}
	return &http.Client{Transport: transport}, nil
}
