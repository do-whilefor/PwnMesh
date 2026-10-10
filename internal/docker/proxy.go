package docker

import (
	"context"
	"errors"
	"io"
	"net"
	"net/url"
	"strings"
	"time"

	"pwnmesh/internal/provider"
)

// Older project containers predate the host-gateway entry. Preserve their
// workspace and only add a missing host alias, using the dispatcher's gateway
// resolution. New containers already receive ExtraHosts from ensure.
func (c *Client) ensureProxyHost(ctx context.Context, name, raw string) error {
	proxy, err := provider.ProxyURL(raw, true)
	if err != nil {
		return err
	}
	u, _ := url.Parse(proxy)
	if !strings.EqualFold(u.Hostname(), "host.docker.internal") {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	unlock := c.lock("proxy-host:" + name)
	defer unlock()
	var info struct {
		HostConfig struct {
			ExtraHosts []string `json:"ExtraHosts"`
		} `json:"HostConfig"`
	}
	if err = c.json(ctx, "GET", "/containers/"+url.PathEscape(name)+"/json", nil, &info); err != nil {
		return err
	}
	for _, host := range info.HostConfig.ExtraHosts {
		if strings.HasPrefix(host, "host.docker.internal:") {
			return nil
		}
	}
	if _, err = c.exec(ctx, name, []string{"getent", "hosts", "host.docker.internal"}, nil, io.Discard); err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	lookup := c.lookupProxyHost
	if lookup == nil {
		lookup = net.DefaultResolver.LookupIPAddr
	}
	addresses, err := lookup(ctx, "host.docker.internal")
	if err != nil {
		return errors.New("cannot resolve Docker host for the existing worker; configure dispatcher host.docker.internal:host-gateway")
	}
	var ip net.IP
	for _, address := range addresses {
		if address.IP == nil || address.IP.IsLoopback() || address.IP.IsUnspecified() || address.IP.IsMulticast() {
			continue
		}
		ip = address.IP
		if ip.To4() != nil {
			break
		}
	}
	if ip == nil {
		return errors.New("Docker host resolved to no usable gateway address")
	}
	// The only variable is a parsed IP passed as an argv value, never shell text.
	const repair = `if ! getent hosts host.docker.internal >/dev/null 2>&1; then printf '\n%s\thost.docker.internal\n' "$1" >> /etc/hosts; fi; getent hosts host.docker.internal >/dev/null`
	if _, err = c.exec(ctx, name, []string{"/bin/sh", "-c", repair, "pwnmesh-proxy-host", ip.String()}, nil, io.Discard); err != nil {
		return errors.New("cannot add Docker host alias to existing worker; preserve its workspace and migrate the container with host.docker.internal:host-gateway")
	}
	return nil
}
