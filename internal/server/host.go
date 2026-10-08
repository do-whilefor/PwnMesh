package server

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"

	"pwnmesh/internal/board"
)

// HostGuard rejects DNS rebinding before serving either API data or the UI.
// Localhost and IP literals work by default; other names require an explicit
// exact match. Origin and proxy headers cannot add trusted names.
func HostGuard(allowed ...string) (func(http.Handler) http.Handler, error) {
	names := map[string]bool{"localhost": true}
	for _, value := range allowed {
		name, ok := hostname(value)
		if !ok {
			return nil, fmt.Errorf("invalid trusted host %q: use a hostname or IP address without a port", value)
		}
		names[name] = true
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			name, ok := requestHostname(r.Host)
			_, ipErr := netip.ParseAddr(name)
			if !ok || (!names[name] && ipErr != nil) {
				writeError(w, r, board.Err(http.StatusForbidden, "Untrusted Host"))
				return
			}
			next.ServeHTTP(w, r)
		})
	}, nil
}

func requestHostname(authority string) (string, bool) {
	name := authority
	if strings.HasPrefix(authority, "[") && strings.HasSuffix(authority, "]") {
		name = authority[1 : len(authority)-1]
	} else if strings.Contains(authority, ":") {
		var port string
		var err error
		name, port, err = net.SplitHostPort(authority)
		if err != nil || port == "" {
			return "", false
		}
		for _, ch := range port {
			if ch < '0' || ch > '9' {
				return "", false
			}
		}
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return "", false
		}
	}
	if strings.HasPrefix(authority, "[") {
		ip, err := netip.ParseAddr(name)
		if err != nil || !ip.Is6() || ip.Zone() != "" {
			return "", false
		}
	}
	return hostname(name)
}

func hostname(name string) (string, bool) {
	if ip, err := netip.ParseAddr(name); err == nil {
		return ip.String(), ip.Zone() == ""
	}
	name = strings.ToLower(strings.TrimSuffix(name, "."))
	if name == "" || len(name) > 253 {
		return "", false
	}
	for _, label := range strings.Split(name, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", false
		}
		for _, ch := range label {
			if !(ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || ch == '-') {
				return "", false
			}
		}
	}
	return name, true
}
