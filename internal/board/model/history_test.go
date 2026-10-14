package model

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestHistoryIsLiteralExpandableAndDoesNotMutateSnapshot(t *testing.T) {
	state := readStateFixture(t, "history.json")
	before, _ := json.Marshal(state)
	history := state.History()
	if len(history) != 1 {
		t.Fatalf("cold groups=%+v", history)
	}
	entry := history[0]
	if entry.ID != "old" || entry.Status != "completed" || !entry.TextTruncated || !utf8.ValidString(entry.Summary) || len(entry.Summary) > 240 || !strings.HasPrefix(state.Steps[0].Description, entry.Summary) {
		t.Fatalf("summary was not a bounded literal prefix: %+v", entry)
	}
	if !slices.Equal(entry.AssetIDs, []string{"asset-a", "asset-b"}) || len(entry.Members) != 3 {
		t.Fatalf("membership or asset union=%+v", entry)
	}
	for i, id := range []string{"old", "observation", "side-observation"} {
		if entry.Members[i].ID != id {
			t.Fatalf("member %d=%+v", i, entry.Members[i])
		}
	}
	observation := entry.Members[1]
	if observation.Kind != "fact" || observation.Status != "valid" || !observation.TextTruncated || !observation.ScopeTruncated || observation.ObservedAt != state.FactRecords[2].ObservedAt || !utf8.ValidString(observation.Excerpt) || !strings.HasPrefix(state.FactRecords[2].Description, observation.Excerpt) || !strings.HasPrefix(state.FactRecords[2].Scope, observation.Scope) {
		t.Fatalf("observation qualifiers were lost or invented: %+v", observation)
	}
	if !reflect.DeepEqual(history, state.History()) {
		t.Fatal("history is not deterministic")
	}
	history[0].Members[0].ID = "changed"
	history[0].AssetIDs[0] = "changed"
	after, _ := json.Marshal(state)
	if string(before) != string(after) {
		t.Fatal("reading or editing history changed the frozen state")
	}
}

func TestHistoryProtectsLiveSupportAndUnresolvedConditions(t *testing.T) {
	cases := map[string]func(*State){
		"goal source":        func(s *State) { s.Goals[0].Sources = []string{"observation"} },
		"candidate source":   func(s *State) { s.Candidates = []Candidate{{ID: "note", Sources: []string{"observation"}}} },
		"candidate producer": func(s *State) { s.Candidates = []Candidate{{ID: "note", SourceStepID: "old"}} },
		"finding source":     func(s *State) { s.Findings = []Finding{{ID: "finding", Sources: []string{"observation"}}} },
		"dispute source": func(s *State) {
			s.Disputes = []Dispute{{ID: "dispute", Status: "open", ReviewFactIDs: []string{"observation"}}}
		},
		"dispute step": func(s *State) {
			s.Disputes = []Dispute{{ID: "dispute", Status: "open", ReviewStepIDs: []string{"old"}}}
		},
		"dispute reference": func(s *State) {
			s.Disputes = []Dispute{{ID: "dispute", Status: "open"}}
			s.Steps[0].DisputeID = "dispute"
		},
		"correction chain": func(s *State) {
			s.FactRelations = []FactRelation{{Kind: "narrows", Source: "new", Target: "observation"}}
		},
		"unsupported result":  func(s *State) { s.FactRecords[2].SupportInvalid = true },
		"refuted observation": func(s *State) { s.FactRecords[2].Status = "refuted" },
		"unsupported step":    func(s *State) { s.Steps[0].SupportValid = false },
		"invalid premise":     func(s *State) { s.Steps[0].InvalidSources = []string{"source"} },
		"blocked dependency":  func(s *State) { s.Steps[0].BlockedBy = []string{"upstream"} },
		"running consumer": func(s *State) {
			s.Steps = append(s.Steps, Step{ID: "current", Status: "running", From: []string{"observation"}})
		},
		"failed consumer": func(s *State) {
			s.Steps = append(s.Steps, Step{ID: "failed", Status: "failed", From: []string{"observation"}})
		},
		"transitive execution": func(s *State) {
			s.Steps = append(s.Steps, Step{ID: "middle", Status: "completed", SupportValid: true, DependsOn: []string{"old"}}, Step{ID: "current", Status: "running", DependsOn: []string{"middle"}})
		},
		"curation request": func(s *State) {
			s.Curation = CurationProgress{Generation: 1, Request: &CurationRequest{Revision: 1, Sources: []string{"observation"}}}
		},
		"root completion": func(s *State) { goal := "goal"; s.Steps[0].Result = &goal },
	}
	for name, modify := range cases {
		t.Run(name, func(t *testing.T) {
			state := readStateFixture(t, "history.json")
			modify(&state)
			for _, entry := range state.History() {
				if entry.ID == "old" {
					t.Fatalf("folded required historical support: %+v", entry)
				}
			}
		})
	}
}

func TestHistoryPreservesFeedbackAndAmbiguousLegacyOwnership(t *testing.T) {
	state := readStateFixture(t, "history.json")
	state.Graph.Project.OrchestrationVersion = 0
	state.FactRecords[2].SourceStepID = ""
	state.Steps = append(state.Steps, Step{ID: "other", Status: "completed", Result: state.Steps[0].Result})
	for _, entry := range state.History() {
		for _, member := range entry.Members {
			if member.ID == "observation" {
				t.Fatal("ambiguous legacy provenance was inferred")
			}
		}
	}
	state.Goals[0].Sources = []string{"observation"}
	if len(state.History()) != 0 {
		t.Fatal("possible producers of active evidence were hidden")
	}
	state = readStateFixture(t, "history.json")
	state.Steps[0].Description = "external_feedback"
	state.FactRecords[2].Legacy, state.FactRecords[2].SourceStepID = true, ""
	if len(state.History()) != 0 {
		t.Fatal("user feedback was folded as old work")
	}
}
