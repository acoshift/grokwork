package sessionstore

import (
	"errors"
	"testing"
)

func TestSameErrorDeploysIsFourTuple(t *testing.T) {
	a := TrackedError{Provider: ErrorProviderDeploys, ID: "iss1", Location: "loc-a", Resource: "api"}
	b := TrackedError{Provider: ErrorProviderDeploys, ID: "iss1", Location: "loc-b", Resource: "api"}
	if sameError(a, b) {
		t.Fatal("same id different location must not alias")
	}
	c := TrackedError{Provider: ErrorProviderDeploys, ID: "iss1", Location: "loc-a", Resource: "api"}
	if !sameError(a, c) {
		t.Fatal("same 4-tuple")
	}
	if sameError(a, TrackedError{Provider: ErrorProviderSentry, ID: "iss1"}) {
		t.Fatal("cross-provider")
	}
}

func TestSameErrorSentryIDOrShortID(t *testing.T) {
	a := TrackedError{Provider: ErrorProviderSentry, ID: "123", ShortID: "APP-1A"}
	if !sameError(a, TrackedError{Provider: ErrorProviderSentry, ID: "123"}) {
		t.Fatal("id")
	}
	if !sameError(a, TrackedError{Provider: ErrorProviderSentry, ShortID: "app-1a"}) {
		t.Fatal("shortId")
	}
}

func TestUpsertErrorCapRefusesFourth(t *testing.T) {
	var e Entry
	for i := range 3 {
		if err := e.UpsertError(TrackedError{Provider: ErrorProviderGCP, ID: "g" + string(rune('a'+i))}); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.UpsertError(TrackedError{Provider: ErrorProviderGCP, ID: "g-new"}); !errors.Is(err, ErrTooManyTrackedErrors) {
		t.Fatalf("got %v", err)
	}
	if len(e.Errors) != 3 {
		t.Fatal(len(e.Errors))
	}
	// Update existing does not count as a 4th.
	if err := e.UpsertError(TrackedError{Provider: ErrorProviderGCP, ID: "ga", Title: "t"}); err != nil {
		t.Fatal(err)
	}
	got, ok := e.FindError("ga")
	if !ok || got.Title != "t" {
		t.Fatalf("%v %+v", ok, got)
	}
}

func TestRemoveAndClearErrors(t *testing.T) {
	var e Entry
	if err := e.UpsertError(TrackedError{Provider: ErrorProviderSentry, ID: "1", ShortID: "APP-1A"}); err != nil {
		t.Fatal(err)
	}
	if !e.RemoveError("APP-1A") {
		t.Fatal("remove shortId")
	}
	if _, ok := e.FindError("1"); ok {
		t.Fatal("still there")
	}
	if err := e.UpsertError(TrackedError{Provider: ErrorProviderGCP, ID: "x"}); err != nil {
		t.Fatal(err)
	}
	e.ClearErrors()
	if len(e.Errors) != 0 {
		t.Fatal("clear")
	}
}
