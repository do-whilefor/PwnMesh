package server

import (
	"net/http"
	"reflect"
	"strings"
	"testing"

	"pwnmesh/internal/board"
)

func TestPrepareRootAssessmentWhileOptionalWorkExistsAndEnforceAtHTTPBoundary(t *testing.T) {
	f := newCurationHTTPFixture(t)
	producer := f.newIntent()
	optional := f.newIntent()
	result := f.completeStep(producer, "The original requested observation was obtained")
	f.run, f.lease, f.intent = "root-planner", "planner@root-planner", ""
	template := snapshotTemplate(f, "reason")
	saved, job := prepareSnapshot(t, f, template)
	if job.Decision.ClosureProtocol != 1 || job.Decision.CompletionAssessment == nil || job.Decision.CompletionAssessment.StateVersion != job.InputSnapshot.StateVersion {
		t.Fatal("active optional work suppressed the version-bound root assessment")
	}
	before := f.state()
	batch := f.batch(batchAction("step", "", `{"action":"add","from":["origin"],"description":"One more unsolicited review"}`))
	f.decision("preview", batch, http.StatusUnprocessableEntity)
	f.decision("commit", batch, http.StatusUnprocessableEntity)
	if !reflect.DeepEqual(before, f.state()) {
		t.Fatal("unassessed HTTP expansion changed state")
	}
	batch = f.batch(
		batchAction("step", "", `{"action":"abandon","id":"`+optional.ID+`","reason":"This extra task is not needed for the user observation"}`),
		batchAction("complete", "", `{"from":["`+result.Fact.ID+`"],"description":"The requested original observation has direct recorded evidence"}`),
	)
	batch.Assessment = &board.RootAssessment{Status: "satisfied", From: []string{result.Fact.ID}, Description: "The original user goal is satisfied by the recorded observation"}
	preview := f.decision("preview", batch, http.StatusOK)
	if preview.CompletionReview == nil || preview.CompletionReview.Acceptance != "not_checked" || !reflect.DeepEqual(before, f.state()) {
		t.Fatal("root judgment bypassed authoritative preview or mutated state")
	}
	var replay board.Execution
	f.request("POST", f.base()+"/executions/prepare", template, true, http.StatusOK, &replay)
	if !reflect.DeepEqual(saved, replay) {
		t.Fatal("reprepared execution changed its frozen root protocol")
	}
	if !f.decision("commit", batch, http.StatusOK).Completed {
		t.Fatal("explicit optional-work closure did not complete")
	}
}

func TestLargeCompletionPacketFallbackRetainsRootAssessmentProtocol(t *testing.T) {
	f := newCurationHTTPFixture(t)
	producer := f.newIntent()
	for _, name := range []string{"First", "Second", "Third"} {
		f.newIntent(name + strings.Repeat(" retained task context", 600))
	}
	f.completeStep(producer, "The requested observation is recorded")
	f.run, f.lease, f.intent = "large-root-planner", "planner@large-root-planner", ""
	_, job := prepareSnapshot(t, f, snapshotTemplate(f, "reason"))
	if job.Decision.CompletionAssessment != nil || job.Decision.ClosureProtocol != 1 {
		t.Fatal("large coverage did not exercise a root-protocol-preserving fallback")
	}
	batch := f.batch(batchAction("step", "", `{"action":"add","from":["origin"],"description":"Unassessed extra work"}`))
	f.decision("preview", batch, http.StatusUnprocessableEntity)
	f.decision("commit", batch, http.StatusUnprocessableEntity)
}
