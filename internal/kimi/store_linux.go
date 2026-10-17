package kimi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"

	"pwnmesh/internal/modelauth"
)

type Store struct {
	Dir    string
	Client *Client
}

func storageError() error { return &Error{Kind: "storage"} }

func (s *Store) locked(ctx context.Context, id string, action func() error) error {
	if !modelauth.ValidCredentialID(id) {
		return &Error{Kind: "invalid_id"}
	}
	if s.Dir == "" {
		return storageError()
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.MkdirAll(s.Dir, 0700); err != nil {
		return storageError()
	}
	info, err := os.Lstat(s.Dir)
	if err != nil || !info.IsDir() || os.Chmod(s.Dir, 0700) != nil {
		return storageError()
	}
	fd, err := unix.Open(filepath.Join(s.Dir, id+".lock"), unix.O_RDWR|unix.O_CREAT|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return storageError()
	}
	defer unix.Close(fd)
	if unix.Fchmod(fd, 0600) != nil {
		return storageError()
	}
	for {
		err = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			return storageError()
		}
		timer := time.NewTimer(20 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	defer unix.Flock(fd, unix.LOCK_UN)
	if err := ctx.Err(); err != nil {
		return err
	}
	return action()
}

func (s *Store) read(id string) (Credential, error) {
	fd, err := unix.Open(filepath.Join(s.Dir, id+".json"), unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if errors.Is(err, unix.ENOENT) {
		return Credential{}, ErrLoginRequired
	}
	if err != nil {
		return Credential{}, storageError()
	}
	file := os.NewFile(uintptr(fd), "credential")
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxBody || file.Chmod(0600) != nil {
		return Credential{}, storageError()
	}
	body, err := io.ReadAll(io.LimitReader(file, maxBody+1))
	if err != nil || len(body) > maxBody {
		return Credential{}, storageError()
	}
	var credential Credential
	if json.Unmarshal(body, &credential) != nil || !validCredential(credential) {
		return Credential{}, storageError()
	}
	return credential, nil
}

func (s *Store) syncDir() error {
	dir, err := os.Open(s.Dir)
	if err != nil {
		return storageError()
	}
	defer dir.Close()
	if dir.Sync() != nil {
		return storageError()
	}
	return nil
}

func (s *Store) save(id string, credential Credential) error {
	if !validCredential(credential) {
		return storageError()
	}
	body, err := json.Marshal(credential)
	if err != nil || len(body) > maxBody {
		return storageError()
	}
	file, err := os.CreateTemp(s.Dir, "."+id+".tmp-")
	if err != nil {
		return storageError()
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if _, err := file.Write(body); err != nil {
		return storageError()
	}
	if file.Sync() != nil || file.Close() != nil {
		return storageError()
	}
	if os.Rename(file.Name(), filepath.Join(s.Dir, id+".json")) != nil {
		return storageError()
	}
	return s.syncDir()
}

func (s *Store) remove(id string) error {
	err := os.Remove(filepath.Join(s.Dir, id+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return storageError()
	}
	return s.syncDir()
}

func (s *Store) Save(id string, credential Credential) error {
	return s.locked(context.Background(), id, func() error { return s.save(id, credential) })
}

func (s *Store) Read(ctx context.Context, id string) (credential Credential, err error) {
	err = s.locked(ctx, id, func() error {
		credential, err = s.read(id)
		return err
	})
	return
}

func (s *Store) Delete(id string) error {
	// The lock inode remains in place so concurrent processes share one lock.
	return s.locked(context.Background(), id, func() error { return s.remove(id) })
}

func (s *Store) Resolve(ctx context.Context, id string) (credential Credential, err error) {
	err = s.locked(ctx, id, func() error {
		current, readErr := s.read(id)
		if readErr != nil {
			return readErr
		}
		if current.ExpiresAt.After(time.Now().Add(60 * time.Second)) {
			credential = current
			return nil
		}
		client := s.Client
		if client == nil {
			client = &Client{}
		}
		next, refreshErr := client.Refresh(ctx, current.RefreshToken)
		if errors.Is(refreshErr, ErrLoginRequired) {
			if err := s.remove(id); err != nil {
				return err
			}
			return ErrLoginRequired
		}
		if refreshErr != nil {
			return refreshErr
		}
		if err := s.save(id, next); err != nil {
			return err
		}
		credential = next
		return nil
	})
	return
}
