package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
)

const MaxActionAssets = 32

// AssetSpec describes identity only. Authentication, timing and observations
// remain in the existing evidence records, never in an asset's identity.
type AssetSpec struct {
	Kind   string `json:"kind"`
	Value  string `json:"value"`
	Method string `json:"method,omitempty"`
}

type Asset struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	Value  string `json:"value"`
	Method string `json:"method,omitempty"`
}

type AssetAnchor struct {
	AssetID  string `json:"asset_id"`
	NodeKind string `json:"node_kind"`
	NodeID   string `json:"node_id"`
}

// NormalizeAsset only merges identities whose equivalence is defined by the
// address syntax. Paths, query order, escaping and HTTP method case survive.
func NormalizeAsset(spec AssetSpec) (Asset, error) {
	a := Asset{Kind: spec.Kind, Value: strings.TrimSpace(spec.Value), Method: spec.Method}
	if a.Value == "" || len(a.Value) > 4096 || strings.ContainsAny(a.Value, "\r\n\t ") {
		return Asset{}, Err(422, "asset value must be a valid address of at most 4096 bytes")
	}
	if a.Kind != "endpoint" && a.Method != "" {
		return Asset{}, Err(422, "asset method is only valid for an endpoint")
	}
	switch a.Kind {
	case "host":
		host, err := canonicalAssetHost(a.Value)
		if err != nil {
			return Asset{}, err
		}
		a.Value = host
	case "service", "endpoint":
		u, err := url.Parse(a.Value)
		if err != nil || u.Host == "" || u.User != nil || u.Opaque != "" || u.Fragment != "" || strings.Contains(a.Value, "#") {
			return Asset{}, Err(422, "asset value must be an absolute URL without userinfo or fragment")
		}
		u.Scheme = strings.ToLower(u.Scheme)
		if strings.Count(u.Host, ":") > 1 && !strings.HasPrefix(u.Host, "[") {
			return Asset{}, Err(422, "IPv6 URLs require a bracketed host")
		}
		if u.Scheme != "http" && u.Scheme != "https" && !(a.Kind == "service" && (u.Scheme == "tcp" || u.Scheme == "udp")) {
			return Asset{}, Err(422, "endpoint scheme must be http/https; service also supports tcp/udp")
		}
		host, err := canonicalAssetHost(u.Hostname())
		if err != nil {
			return Asset{}, err
		}
		if strings.HasPrefix(u.Host, "[") && !strings.Contains(host, ":") {
			return Asset{}, Err(422, "bracketed asset hosts must be IPv6 addresses")
		}
		port := u.Port()
		if strings.HasSuffix(u.Host, ":") {
			return Asset{}, Err(422, "asset port must not be empty")
		}
		if port != "" {
			n, err := strconv.Atoi(port)
			if err != nil || n < 1 || n > 65535 {
				return Asset{}, Err(422, "asset port must be between 1 and 65535")
			}
			port = strconv.Itoa(n)
			if u.Scheme == "http" && n == 80 || u.Scheme == "https" && n == 443 {
				port = ""
			}
		} else if u.Scheme == "tcp" || u.Scheme == "udp" {
			return Asset{}, Err(422, "tcp/udp service requires an explicit port")
		}
		u.Host = host
		if strings.Contains(host, ":") {
			u.Host = "[" + host + "]"
		}
		if port != "" {
			u.Host = net.JoinHostPort(host, port)
		}
		if a.Kind == "service" {
			if u.Path != "" && u.Path != "/" || u.RawPath != "" || u.RawQuery != "" || u.ForceQuery {
				return Asset{}, Err(422, "service value must be an origin without path or query")
			}
			u.Path = ""
		} else {
			if !validAssetMethod(a.Method) {
				return Asset{}, Err(422, "endpoint requires an HTTP method token of at most 32 bytes")
			}
			if u.Path == "" {
				u.Path = "/"
			}
		}
		a.Value = u.String()
		if len(a.Value) > 4096 {
			return Asset{}, Err(422, "normalized asset value must be at most 4096 bytes")
		}
	default:
		return Asset{}, Err(422, "asset kind must be host, service or endpoint")
	}
	raw, _ := json.Marshal([]string{a.Kind, a.Value, a.Method})
	digest := sha256.Sum256(raw)
	a.ID = "asset_" + hex.EncodeToString(digest[:16])
	return a, nil
}

func canonicalAssetHost(value string) (string, error) {
	if address, err := netip.ParseAddr(value); err == nil {
		if address.Zone() != "" {
			return "", Err(422, "asset host must not contain an IPv6 zone")
		}
		return address.String(), nil
	}
	host := strings.ToLower(strings.TrimSuffix(value, "."))
	if host == "" || len(host) > 253 {
		return "", Err(422, "invalid asset host")
	}
	if strings.Contains(host, ".") && strings.Trim(host, "0123456789.") == "" {
		return "", Err(422, "invalid IPv4 asset host")
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", Err(422, "invalid asset host")
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return "", Err(422, "asset host must be an IP address or ASCII DNS name")
			}
		}
	}
	return host, nil
}

func validAssetMethod(method string) bool {
	if method == "" || len(method) > 32 {
		return false
	}
	for _, c := range method {
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", c)) {
			return false
		}
	}
	return true
}
