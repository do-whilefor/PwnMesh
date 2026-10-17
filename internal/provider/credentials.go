package provider

import (
	"context"
	"errors"
	"net/http"
	"sync"

	"pwnmesh/internal/agent"
	"pwnmesh/internal/kimi"
)

func ConnectionClient(mode, proxy string) (*http.Client, error) { return connectionClient(mode, proxy) }

func (p *Client) configureKimi(dir, id string) error {
	store := &kimi.Store{Dir: dir, Client: &kimi.Client{HTTP: p.Client}}
	initial, err := store.Read(id)
	if err != nil {
		return err
	}
	p.Token = initial.AccessToken
	var mu sync.Mutex
	secrets := []string{initial.AccessToken, initial.RefreshToken}
	p.TokenSource = func(ctx context.Context) (string, error) {
		credential, err := store.Resolve(ctx, id)
		if err != nil {
			return "", classifyCredentialError(err)
		}
		mu.Lock()
		for _, value := range []string{credential.AccessToken, credential.RefreshToken} {
			found := false
			for _, known := range secrets {
				found = found || known == value
			}
			if !found && value != "" {
				secrets = append(secrets, value)
			}
		}
		mu.Unlock()
		return credential.AccessToken, nil
	}
	p.Secrets = func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), secrets...) }
	return nil
}

func classifyCredentialError(err error) error {
	var auth *kimi.Error
	if !errors.As(err, &auth) {
		return err
	}
	kind := agent.ErrorProvider
	switch auth.Kind {
	case "network":
		kind = agent.ErrorTransport
	case "unavailable":
		kind = agent.ErrorUnavailable
	case "rate_limit":
		kind = agent.ErrorRateLimit
	}
	if auth.Status >= 100 && auth.Status <= 599 {
		err = errors.Join(err, &HTTPError{Status: auth.Status})
	}
	return &agent.ModelError{Kind: kind, Err: err}
}

func (p *Client) CredentialValues() []string {
	values := []string{p.Token}
	if p.Secrets != nil {
		values = append(values, p.Secrets()...)
	}
	return values
}
