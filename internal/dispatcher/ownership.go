package dispatcher

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"time"
)

// Fake/in-process runners need no Docker ownership. Container runners bind to
// the authoritative database before any startup cleanup or scheduling tick.
func (s *Scheduler) bindOwnership(ctx context.Context) error {
	runner, ok := s.Runner.(interface{ BindOwner(string) error })
	if !ok {
		return nil
	}
	var deployment struct {
		ID string `json:"installation_id"`
	}
	if err := s.Client.Do(ctx, "GET", "/deployment", nil, &deployment, nil); err != nil {
		return err
	}
	return runner.BindOwner(deployment.ID)
}

func (s *Scheduler) acquireDispatcher(ctx context.Context) (context.Context, func(), error) {
	// In-process test runners do not own or clean Docker namespaces.
	if _, ok := s.Runner.(interface{ BindOwner(string) error }); !ok {
		return ctx, func() {}, nil
	}
	var deployment struct {
		ID string `json:"installation_id"`
	}
	if err := s.Client.Do(ctx, "GET", "/deployment", nil, &deployment, nil); err != nil {
		return nil, nil, err
	}
	if err := s.Runner.(interface{ BindOwner(string) error }).BindOwner(deployment.ID); err != nil {
		return nil, nil, err
	}
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return nil, nil, err
	}
	body := map[string]string{"holder": hex.EncodeToString(token[:]), "installation_id": deployment.ID}
	base := "/dispatchers/" + url.PathEscape(s.namespace()) + "/"
	if err := s.Client.Do(ctx, "POST", base+"claim", body, nil, nil); err != nil {
		return nil, nil, err
	}
	owned, cancel := context.WithCancelCause(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(5 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-owned.Done():
				return
			case <-tick.C:
				call, stop := context.WithTimeout(owned, 3*time.Second)
				err := s.Client.Do(call, "POST", base+"heartbeat", body, nil, nil)
				stop()
				if err != nil {
					cancel(fmt.Errorf("dispatcher ownership lost: %w", err))
					return
				}
			}
		}
	}()
	return owned, func() {
		cancel(context.Canceled)
		<-done
		call, stop := context.WithTimeout(context.Background(), 3*time.Second)
		defer stop()
		_ = s.Client.Do(call, "POST", base+"release", body, nil, nil)
	}, nil
}
