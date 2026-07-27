package sessionstore

import (
	"reflect"
	"testing"
)

// TestEntryCloneDetachesEveryField is a reflection guard, not a hand-written
// field list: it walks Entry and fails when any slice, map or pointer field
// still shares memory with the original after clone(). Adding a mutable field
// to Entry (or to Dossier / OpenQuestion) without extending clone() fails here
// instead of silently reintroducing the write-through Store.Get used to have.
func TestEntryCloneDetachesEveryField(t *testing.T) {
	orig := cloneFixture()

	got := orig.clone()

	// Value equality must hold — clone detaches memory, it does not drop data.
	if !reflect.DeepEqual(orig, got) {
		t.Fatalf("clone changed value:\n orig=%+v\n got =%+v", orig, got)
	}

	assertDetached(t, reflect.ValueOf(orig), reflect.ValueOf(got), "Entry")
}

// TestCloneFixtureCoversEveryReferenceField is what makes the guard above a
// guard at all.
//
// assertDetached returns early on a nil or empty slice, map or pointer: there is
// no backing array to share, so nothing can be proven about it. That means a new
// Entry field left out of the fixture passes the detachment walk *trivially*,
// even when clone() never learned to copy it — the exact failure the walk exists
// to catch, silently exempted by the omission it should have flagged.
//
// So the fixture's completeness is asserted separately: every exported
// reference-typed field, at every depth clone() reaches, must be populated.
// Adding `Foo []string` to Entry now fails here with the field name until the
// fixture names it, and only then does the detachment walk get to say whether
// clone() handles it.
func TestCloneFixtureCoversEveryReferenceField(t *testing.T) {
	assertPopulated(t, reflect.ValueOf(cloneFixture()), "Entry")
}

// assertPopulated fails for every exported slice/map/pointer field that is nil
// or empty, recursing through pointers and into slice elements.
func assertPopulated(t *testing.T, v reflect.Value, path string) {
	t.Helper()
	switch v.Kind() {
	case reflect.Struct:
		for i := range v.NumField() {
			if !v.Type().Field(i).IsExported() {
				continue
			}
			assertPopulated(t, v.Field(i), path+"."+v.Type().Field(i).Name)
		}
	case reflect.Slice:
		if v.IsNil() || v.Len() == 0 {
			t.Errorf("%s: clone fixture leaves this slice empty, so the detachment walk skips it — populate it in cloneFixture()", path)
			return
		}
		// One element is enough to prove the backing array is detached; walking
		// it catches nested reference fields (OpenQuestion.Options).
		assertPopulated(t, v.Index(0), path+"[0]")
	case reflect.Map:
		if v.IsNil() || v.Len() == 0 {
			t.Errorf("%s: clone fixture leaves this map empty, so the detachment walk skips it — populate it in cloneFixture()", path)
		}
	case reflect.Pointer:
		if v.IsNil() {
			t.Errorf("%s: clone fixture leaves this pointer nil, so the detachment walk skips it — populate it in cloneFixture()", path)
			return
		}
		assertPopulated(t, v.Elem(), path+"*")
	}
}

// cloneFixture is an Entry with every exported reference-typed field populated,
// at every depth. TestCloneFixtureCoversEveryReferenceField enforces that.
func cloneFixture() Entry {
	return Entry{
		SessionID: "s1",
		Project:   "p",
		// SLA clocks are scalars, so the detachment walk below has nothing to
		// check for them — the DeepEqual is what proves clone() still carries
		// them, which is the way a new Entry field usually gets lost.
		OpenedAt:        "2026-07-27T09:00:00Z",
		FirstResponseAt: "2026-07-27T09:20:00Z",
		AnsweredAt:      "2026-07-27T11:00:00Z",
		CoOwnerIDs:      []string{"a", "b"},
		Issues:          []TrackedIssue{{Number: 1, Owner: "o", Repo: "r"}},
		PRs:             []TrackedPR{{Number: 2, Owner: "o", Repo: "r"}},
		RelatedCases:    []string{"WEBAPP-1"},
		Checkpoints:     []CheckpointMeta{{ID: "c1", Ref: "refs/x", SHA: "abc"}},
		WatcherIDs:      []string{"w1"},
		OpenQuestions:   []OpenQuestion{{ID: "q1", Text: "t", Options: []string{"yes", "no"}}},
		Discord:         &DiscordRef{ThreadID: "th", URL: "u"},
		LastVerify:      &LastVerify{Name: "unit", OK: true},
		Dossier: &Dossier{
			Summary:      "s",
			ReproSteps:   []string{"step"},
			Evidence:     []string{"ev"},
			Hypotheses:   []string{"hy"},
			KnownBugHits: []string{"kb"},
			NextActions:  []string{"na"},
		},
	}
}

// assertDetached walks two values of the same type and fails on any shared
// slice backing array, map header or pointer target.
func assertDetached(t *testing.T, a, b reflect.Value, path string) {
	t.Helper()
	switch a.Kind() {
	case reflect.Struct:
		for i := range a.NumField() {
			if !a.Type().Field(i).IsExported() {
				continue
			}
			assertDetached(t, a.Field(i), b.Field(i), path+"."+a.Type().Field(i).Name)
		}
	case reflect.Slice:
		if a.IsNil() || a.Len() == 0 {
			return
		}
		if a.UnsafePointer() == b.UnsafePointer() {
			t.Errorf("%s: slice shares backing array with the original — extend Entry.clone", path)
			return
		}
		for i := range a.Len() {
			assertDetached(t, a.Index(i), b.Index(i), path+"[]")
		}
	case reflect.Map:
		if a.IsNil() {
			return
		}
		if a.UnsafePointer() == b.UnsafePointer() {
			t.Errorf("%s: map shares its header with the original — extend Entry.clone", path)
		}
	case reflect.Pointer:
		if a.IsNil() {
			return
		}
		if a.Pointer() == b.Pointer() {
			t.Errorf("%s: pointer still targets the original — extend Entry.clone", path)
			return
		}
		assertDetached(t, a.Elem(), b.Elem(), path+"*")
	}
}

// TestStoreGetDoesNotAliasStoredEntry is the behavioural half: mutating what
// Get returned must not change what the store holds. Before clone-on-read this
// wrote straight through, bypassing both the lock and the save.
func TestStoreGetDoesNotAliasStoredEntry(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Set("t1", Entry{
		SessionID: "s1",
		Project:   "p",
		PRs:       []TrackedPR{{Number: 1, State: "OPEN", Owner: "o", Repo: "r"}},
		Dossier:   &Dossier{Summary: "before", ReproSteps: []string{"one"}},
	}); err != nil {
		t.Fatal(err)
	}

	got, ok := s.Get("t1")
	if !ok {
		t.Fatal("missing entry")
	}
	got.PRs[0].State = "MERGED"
	got.Dossier.Summary = "after"
	got.Dossier.ReproSteps[0] = "mutated"

	again, ok := s.Get("t1")
	if !ok {
		t.Fatal("missing entry")
	}
	if again.PRs[0].State != "OPEN" {
		t.Errorf("PR state written through: got %q, want OPEN", again.PRs[0].State)
	}
	if again.Dossier.Summary != "before" {
		t.Errorf("Dossier written through: got %q, want before", again.Dossier.Summary)
	}
	if again.Dossier.ReproSteps[0] != "one" {
		t.Errorf("Dossier slice written through: got %q, want one", again.Dossier.ReproSteps[0])
	}
}

// TestStoreSetDoesNotAliasCallerEntry is the inbound half: the store must not
// keep the caller's slices, or a caller mutating its own copy later corrupts
// store state without going through Patch.
func TestStoreSetDoesNotAliasCallerEntry(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mine := Entry{
		SessionID:  "s1",
		Project:    "p",
		CoOwnerIDs: []string{"alice"},
	}
	if err := s.Set("t1", mine); err != nil {
		t.Fatal(err)
	}

	mine.CoOwnerIDs[0] = "attacker"

	got, ok := s.Get("t1")
	if !ok {
		t.Fatal("missing entry")
	}
	if got.CoOwnerIDs[0] != "alice" {
		t.Errorf("caller mutation reached the store: got %q, want alice", got.CoOwnerIDs[0])
	}
}
