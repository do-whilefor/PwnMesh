package kimi

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

const fixtureID = "0123456789abcdef0123456789abcdef"

func fixtureCredential() Credential {
	return Credential{AccessToken: "old-access", RefreshToken: "old-refresh", ExpiresAt: time.Now().UTC().Add(30 * time.Second)}
}

func TestStoreRoundTripPermissionsAndIdentifierValidation(t *testing.T) {
	store := &Store{Dir: filepath.Join(t.TempDir(), "credentials")}
	credential := fixtureCredential()
	if err := store.Save(fixtureID, credential); err != nil {
		t.Fatal(err)
	}
	got, err := store.Read(fixtureID)
	if err != nil || got.AccessToken != credential.AccessToken || got.RefreshToken != credential.RefreshToken || !got.ExpiresAt.Equal(credential.ExpiresAt) {
		t.Fatalf("stored credential changed: %v", err)
	}
	for _, tc := range []struct {
		path string
		mode os.FileMode
	}{{store.Dir, 0700}, {filepath.Join(store.Dir, fixtureID+".json"), 0600}, {filepath.Join(store.Dir, fixtureID+".lock"), 0600}} {
		info, err := os.Stat(tc.path)
		if err != nil || info.Mode().Perm() != tc.mode {
			t.Fatal("credential storage permissions are not private")
		}
	}
	entries, _ := os.ReadDir(store.Dir)
	if len(entries) != 2 {
		t.Fatal("atomic save left temporary credential files")
	}
	for _, id := range []string{"", "../" + fixtureID, "zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz", fixtureID + "0"} {
		if err := store.Save(id, credential); err == nil {
			t.Fatal("invalid credential identifier accepted")
		}
		if _, err := store.Read(id); err == nil {
			t.Fatal("invalid credential identifier read")
		}
		if err := store.Delete(id); err == nil {
			t.Fatal("invalid credential identifier deleted")
		}
	}
	if err := store.Delete(fixtureID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Read(fixtureID); !errors.Is(err, ErrLoginRequired) {
		t.Fatalf("deleted credential remained available: %v", err)
	}
}

func TestStoreConcurrentResolveRefreshesOnceAndPersistsRotation(t *testing.T) {
	var refreshes atomic.Int32
	client := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		refreshes.Add(1)
		if r.ParseForm() != nil || r.Form.Get("refresh_token") != "old-refresh" {
			t.Error("refresh reused a rotated credential")
		}
		time.Sleep(30 * time.Millisecond)
		io.WriteString(w, `{"access_token":"new-access","refresh_token":"new-refresh","expires_in":3600}`)
	})
	store := &Store{Dir: t.TempDir(), Client: client}
	if err := store.Save(fixtureID, fixtureCredential()); err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	for i := 0; i < 8; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			credential, err := store.Resolve(context.Background(), fixtureID)
			if err != nil || credential.AccessToken != "new-access" || credential.RefreshToken != "new-refresh" {
				t.Errorf("concurrent refresh failed: %v", err)
			}
		}()
	}
	group.Wait()
	credential, err := store.Read(fixtureID)
	if err != nil || credential.RefreshToken != "new-refresh" || refreshes.Load() != 1 {
		t.Fatalf("rotation not persisted once: requests=%d err=%v", refreshes.Load(), err)
	}
}

func TestStoreCrossProcessResolve(t *testing.T) {
	if os.Getenv("PWNMESH_TEST_KIMI_CHILD") == "1" {
		store := &Store{Dir: os.Getenv("PWNMESH_TEST_KIMI_DIR"), Client: &Client{BaseURL: os.Getenv("PWNMESH_TEST_KIMI_URL")}}
		credential, err := store.Resolve(context.Background(), fixtureID)
		if err != nil || credential.AccessToken != "new-access" || credential.RefreshToken != "new-refresh" {
			t.Fatalf("child could not resolve credential: %v", err)
		}
		return
	}
	var refreshes atomic.Int32
	client := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		refreshes.Add(1)
		time.Sleep(40 * time.Millisecond)
		io.WriteString(w, `{"access_token":"new-access","refresh_token":"new-refresh","expires_in":3600}`)
	})
	store := &Store{Dir: t.TempDir(), Client: client}
	if err := store.Save(fixtureID, fixtureCredential()); err != nil {
		t.Fatal(err)
	}
	var children []*exec.Cmd
	var outputs []*bytes.Buffer
	for i := 0; i < 2; i++ {
		cmd := exec.Command(os.Args[0], "-test.run=^TestStoreCrossProcessResolve$", "-test.timeout=10s")
		cmd.Env = append(os.Environ(), "PWNMESH_TEST_KIMI_CHILD=1", "PWNMESH_TEST_KIMI_DIR="+store.Dir, "PWNMESH_TEST_KIMI_URL="+client.BaseURL)
		output := &bytes.Buffer{}
		cmd.Stdout, cmd.Stderr = output, output
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		children = append(children, cmd)
		outputs = append(outputs, output)
	}
	for i, cmd := range children {
		if err := cmd.Wait(); err != nil {
			t.Fatalf("child failed: %v\n%s", err, outputs[i].String())
		}
	}
	if refreshes.Load() != 1 {
		t.Fatalf("cross-process refresh raced: %d requests", refreshes.Load())
	}
}

func TestStorePreservesTransientFailureAndDeletesRevokedCredentials(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		body    string
		revoked bool
	}{
		{"unavailable", 503, `secret-upstream-body`, false},
		{"limited", 429, `{"error":"rate_limit"}`, false},
		{"malformed", 200, `{"access_token":"secret-upstream-body"}`, false},
		{"revoked", 400, `{"error":"invalid_grant","error_description":"secret-upstream-body"}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.status); io.WriteString(w, tc.body) })
			store := &Store{Dir: t.TempDir(), Client: client}
			before := fixtureCredential()
			if err := store.Save(fixtureID, before); err != nil {
				t.Fatal(err)
			}
			if _, err := store.Resolve(context.Background(), fixtureID); err == nil || errors.Is(err, ErrLoginRequired) != tc.revoked {
				t.Fatalf("wrong refresh failure: %v", err)
			}
			after, err := store.Read(fixtureID)
			if tc.revoked {
				if !errors.Is(err, ErrLoginRequired) {
					t.Fatal("revoked credential was retained")
				}
				return
			}
			if err != nil || after.AccessToken != before.AccessToken || after.RefreshToken != before.RefreshToken || !after.ExpiresAt.Equal(before.ExpiresAt) {
				t.Fatal("transient failure changed stored credentials")
			}
		})
	}
}

func TestStoreLockWaitHonorsContextAndRejectsUnsafeFiles(t *testing.T) {
	store := &Store{Dir: t.TempDir()}
	if err := store.Save(fixtureID, fixtureCredential()); err != nil {
		t.Fatal(err)
	}
	lock, err := os.OpenFile(filepath.Join(store.Dir, fixtureID+".lock"), os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := store.Resolve(ctx, fixtureID); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lock wait ignored context: %v", err)
	}
	unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	path := filepath.Join(store.Dir, fixtureID+".json")
	if err := os.WriteFile(path, []byte(`secret-upstream-body`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Read(fixtureID); err == nil {
		t.Fatal("malformed credential accepted")
	}
	outside := filepath.Join(t.TempDir(), "outside.json")
	if err := os.WriteFile(outside, []byte(`secret-outside`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, path); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Read(fixtureID); err == nil {
		t.Fatal("credential symlink followed")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Read(fixtureID); err == nil {
		t.Fatal("nonregular credential file accepted")
	}
}
