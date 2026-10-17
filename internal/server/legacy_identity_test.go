package server

import (
	"context"
	"net/http"
	"reflect"
	"testing"
	"time"

	"pwnmesh/internal/board"
)

func TestUnregisteredClaimsCannotReuseRevokedIdentity(t *testing.T) {
	for _, kind := range []string{"reason", "curate", "explore"} {
		t.Run(kind, func(t *testing.T) {
			f := newExecutionProtocolFixture(t)
			leasePath, claim := f.base()+"/"+kind, "/claim"
			if kind == "explore" {
				leasePath = f.base() + "/intents/" + f.newIntent().ID
				claim = "/heartbeat"
			}
			body := map[string]string{"worker": "unregistered-run", "trigger": "initial"}
			f.request("POST", leasePath+claim, body, false, http.StatusOK, nil)
			if err := f.store.Do(context.Background(), func(tx *board.Tx) error {
				return tx.RevokeRuns(f.project)
			}); err != nil {
				t.Fatal(err)
			}
			before := f.state()
			later := f.store.Now().Add(time.Second)
			f.store.Now = func() time.Time { return later }
			for _, op := range []string{claim, "/heartbeat"} {
				f.request("POST", leasePath+op, body, false, http.StatusConflict, nil)
				if !reflect.DeepEqual(before, f.state()) {
					t.Fatal("revoked unregistered identity changed its lease")
				}
			}
			f.request("POST", leasePath+"/release", body, false, http.StatusOK, nil)
			body["worker"] = "fresh-run"
			f.request("POST", leasePath+claim, body, false, http.StatusOK, nil)
		})
	}
}

func TestRegisteredStepOperationsBindRequestIdentity(t *testing.T) {
	for _, kind := range []string{"reason", "explore"} {
		for _, op := range []string{"heartbeat", "release"} {
			t.Run(kind+"/"+op, func(t *testing.T) {
				f := newExecutionProtocolFixture(t)
				if kind == "reason" {
					intent := f.newIntent()
					f.request("POST", f.base()+"/hints", map[string]string{"creator": "user", "content": "Reassess the pending fixture direction"}, false, http.StatusCreated, nil)
					f.register("reason")
					f.intent = intent.ID
				} else {
					f.register(kind)
					f.request("POST", f.base()+"/intents/"+f.intent+"/release", map[string]string{"worker": f.lease}, true, http.StatusOK, nil)
				}
				before := f.state()
				for _, test := range []struct {
					name   string
					worker any
					status int
				}{
					{"mismatched", "unregistered-worker", http.StatusForbidden},
					{"empty", "", http.StatusUnprocessableEntity},
					{"null", nil, http.StatusUnprocessableEntity},
					{"non-string", 123, http.StatusUnprocessableEntity},
				} {
					t.Run(test.name, func(t *testing.T) {
						f.request("POST", f.base()+"/intents/"+f.intent+"/"+op, map[string]any{"worker": test.worker}, true, test.status, nil)
						if !reflect.DeepEqual(before, f.state()) {
							t.Fatal("invalid identity changed the Step or published a Fact")
						}
					})
				}
			})
		}
	}
}

func TestRegisteredStepOperationCannotUseAnotherStepPath(t *testing.T) {
	f := newExecutionProtocolFixture(t)
	f.register("explore")
	other := f.newIntent()
	before := f.state()
	for _, op := range []string{"heartbeat", "release"} {
		f.request("POST", f.base()+"/intents/"+other.ID+"/"+op, map[string]string{"worker": f.lease}, true, http.StatusForbidden, nil)
	}
	if !reflect.DeepEqual(before, f.state()) {
		t.Fatal("mismatched Step path changed shared state")
	}
}
