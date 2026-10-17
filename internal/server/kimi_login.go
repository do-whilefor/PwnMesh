package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"

	b "pwnmesh/internal/board"
	"pwnmesh/internal/kimi"
	"pwnmesh/internal/modelconfig"
)

type kimiClientFactory func(mode, proxy string) (*kimi.Client, error)

type kimiLogin struct {
	client        *kimi.Client
	device        kimi.Device
	credential    kimi.Credential
	mode, proxy   string
	expires, next time.Time
	interval      int
	inflight      bool
	ctx           context.Context
	cancel        context.CancelFunc
	timer         *time.Timer
}

type kimiLogins struct {
	mu       sync.Mutex
	sessions map[string]*kimiLogin
	factory  kimiClientFactory
}

// remove runs under mu. Authorized drafts never reach disk until save succeeds.
func (l *kimiLogins) remove(id string) {
	if session := l.sessions[id]; session != nil {
		delete(l.sessions, id)
		session.cancel()
		if session.timer != nil {
			session.timer.Stop()
		}
		if session.client.HTTP != nil {
			session.client.HTTP.CloseIdleConnections()
		}
	}
}

func (l *kimiLogins) expire(id string, session *kimiLogin, duration time.Duration) {
	if session.timer != nil {
		session.timer.Stop()
	}
	session.expires = time.Now().Add(duration)
	session.timer = time.AfterFunc(duration, func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		if l.sessions[id] == session && !time.Now().Before(session.expires) {
			l.remove(id)
		}
	})
}

func (l *kimiLogins) register(m *http.ServeMux, configured bool) {
	m.HandleFunc("POST /model-settings/kimi/login", func(w http.ResponseWriter, r *http.Request) {
		if !configured {
			writeError(w, r, b.Err(422, "model settings storage is not configured"))
			return
		}
		raw, err := modelPatch(w, r)
		if err != nil {
			writeError(w, r, b.Err(422, err.Error()))
			return
		}
		connection := struct {
			Mode  string `json:"connection_mode"`
			Proxy string `json:"proxy_url"`
		}{Mode: "direct"}
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		var extra any
		if decoder.Decode(&connection) != nil || decoder.Decode(&extra) != io.EOF || (connection.Mode != "direct" && connection.Mode != "proxy") {
			writeError(w, r, b.Err(422, "invalid sign-in connection settings"))
			return
		}
		client, err := l.factory(connection.Mode, connection.Proxy)
		if err != nil {
			writeError(w, r, b.Err(422, "invalid sign-in connection settings"))
			return
		}
		var nonce [16]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			writeError(w, r, b.Err(503, "cannot start sign-in"))
			return
		}
		id := hex.EncodeToString(nonce[:])
		ctx, cancel := context.WithCancel(context.Background())
		session := &kimiLogin{client: client, mode: connection.Mode, proxy: connection.Proxy, ctx: ctx, cancel: cancel, inflight: true}
		l.mu.Lock()
		if len(l.sessions) >= 8 {
			l.mu.Unlock()
			cancel()
			if client.HTTP != nil {
				client.HTTP.CloseIdleConnections()
			}
			writeError(w, r, b.Err(429, "too many pending Kimi sign-ins"))
			return
		}
		l.sessions[id] = session
		l.expire(id, session, time.Minute)
		l.mu.Unlock()
		requestCtx, requestCancel := context.WithCancel(r.Context())
		stop := context.AfterFunc(ctx, requestCancel)
		device, err := client.Start(requestCtx)
		stop()
		requestCancel()
		l.mu.Lock()
		defer l.mu.Unlock()
		if err != nil || r.Context().Err() != nil || l.sessions[id] != session {
			l.remove(id)
			writeError(w, r, b.Err(502, "Kimi Code sign-in could not be started"))
			return
		}
		session.device, session.interval, session.inflight = device, device.Interval, false
		session.next = time.Now().Add(time.Duration(device.Interval) * time.Second)
		l.expire(id, session, time.Duration(device.ExpiresIn)*time.Second)
		modelJSON(w, map[string]any{"login_id": id, "user_code": device.UserCode, "verification_uri": device.VerificationURI, "expires_in": device.ExpiresIn, "interval": device.Interval})
	})
	m.HandleFunc("POST /model-settings/kimi/login/{id}", l.poll)
	m.HandleFunc("DELETE /model-settings/kimi/login/{id}", func(w http.ResponseWriter, r *http.Request) {
		l.mu.Lock()
		l.remove(r.PathValue("id"))
		l.mu.Unlock()
		modelJSON(w, map[string]string{"status": "cancelled"})
	})
}

func (l *kimiLogins) poll(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	l.mu.Lock()
	session := l.sessions[id]
	if session == nil || !time.Now().Before(session.expires) {
		l.remove(id)
		l.mu.Unlock()
		modelJSON(w, map[string]string{"status": "expired"})
		return
	}
	if session.credential.AccessToken != "" {
		l.mu.Unlock()
		modelJSON(w, map[string]string{"status": "authorized"})
		return
	}
	if session.inflight || time.Now().Before(session.next) {
		interval := session.interval
		l.mu.Unlock()
		modelJSON(w, map[string]any{"status": "pending", "interval": interval})
		return
	}
	session.inflight = true
	l.mu.Unlock()
	ctx, cancel := context.WithCancel(r.Context())
	stop := context.AfterFunc(session.ctx, cancel)
	credential, status, interval, err := session.client.Poll(ctx, session.device.DeviceCode)
	stop()
	cancel()
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.sessions[id] != session || !time.Now().Before(session.expires) {
		l.remove(id)
		modelJSON(w, map[string]string{"status": "expired"})
		return
	}
	session.inflight = false
	if status == "expired" || status == "denied" {
		l.remove(id)
		modelJSON(w, map[string]string{"status": status})
		return
	}
	if err != nil {
		l.remove(id)
		writeError(w, r, b.Err(502, "Kimi Code authorization failed; start sign-in again"))
		return
	}
	if status == "slow_down" {
		session.interval = max(session.interval+5, interval)
	}
	session.next = time.Now().Add(time.Duration(session.interval) * time.Second)
	if status == "authorized" {
		session.credential = credential
		session.device.DeviceCode = ""
		l.expire(id, session, min(15*time.Minute, time.Until(credential.ExpiresAt)))
	}
	modelJSON(w, map[string]any{"status": status, "interval": session.interval})
}

func (l *kimiLogins) authorizedLocked(id string, settings modelconfig.Settings) (*kimiLogin, error) {
	session := l.sessions[id]
	if session == nil || !time.Now().Before(session.expires) || session.credential.AccessToken == "" || !time.Now().Before(session.credential.ExpiresAt) {
		return nil, errors.New("complete Kimi Code sign-in before testing or saving")
	}
	if settings.AuthMode != "kimi_oauth" || settings.ConnectionMode != session.mode || settings.ProxyURL != session.proxy {
		return nil, errors.New("sign-in connection changed; start Kimi Code sign-in again")
	}
	copy := *session
	return &copy, nil
}

func (l *kimiLogins) authorized(id string, settings modelconfig.Settings) (*kimiLogin, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.authorizedLocked(id, settings)
}
