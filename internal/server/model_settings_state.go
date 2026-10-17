package server

import (
	"errors"

	"pwnmesh/internal/kimi"
	"pwnmesh/internal/modelconfig"
)

// modelSettings coordinates persisted configuration and subscription credentials.
// Device-grant sessions own neither settings storage nor ordinary API-key saves.
type modelSettings struct {
	store       *modelconfig.Store
	logins      *kimiLogins
	credentials savedCredentials
}

type savedCredentials interface {
	Read(string) (kimi.Credential, error)
	Save(string, kimi.Credential) error
	Delete(string) error
}

func (m *modelSettings) public(s modelconfig.Settings) modelconfig.Public {
	if s.AuthMode == "kimi_oauth" && s.CredentialID != "" {
		if _, err := m.credentials.Read(s.CredentialID); err != nil {
			s.CredentialID = ""
		}
	}
	return s.Public()
}

func (m *modelSettings) save(raw []byte) (modelconfig.Settings, error) {
	created := ""
	locked := false
	defer func() {
		if locked {
			m.logins.mu.Unlock()
		}
	}()
	current, err := m.store.Update(func(s *modelconfig.Settings) error {
		id, err := decodeModelPatch(raw, s)
		if err != nil || id == "" {
			return err
		}
		// A completed PUT may lose its response. Retrying its login ID must
		// reuse the saved credential, never replay or overwrite rotated tokens.
		if id == s.CredentialID {
			_, err := m.credentials.Read(id)
			return err
		}
		// Always acquire the settings lock first. Keep the draft alive until
		// settings persistence or credential rollback has completed.
		m.logins.mu.Lock()
		locked = true
		session, err := m.logins.authorizedLocked(id, *s)
		if err != nil {
			return err
		}
		s.CredentialID = id
		if err := s.Validate(true); err != nil {
			return err
		}
		// Save can fail after rename (for example, on directory sync).
		// Roll back this draft's file even when Save reports an error.
		created = id
		if err := m.credentials.Save(id, session.credential); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		if created != "" {
			_ = m.credentials.Delete(created)
		}
		return current, err
	}
	if created != "" {
		m.logins.remove(created)
	}
	// Older saved references remain valid for already running tasks.
	return current, nil
}

func (m *modelSettings) logout() (modelconfig.Settings, error) {
	return m.store.Update(func(s *modelconfig.Settings) error {
		if s.AuthMode != "kimi_oauth" {
			return errors.New("no saved Kimi subscription connection")
		}
		if s.CredentialID != "" {
			// Revocation remains effective even if the settings write fails.
			if err := m.credentials.Delete(s.CredentialID); err != nil {
				return err
			}
		}
		s.CredentialID, s.Token = "", ""
		return nil
	})
}
