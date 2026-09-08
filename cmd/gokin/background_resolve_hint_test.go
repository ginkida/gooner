package main

import (
	"errors"
	"strings"
	"testing"

	backgroundstore "gokin/internal/background"
)

// A user reaches an unknown-job error at the moment they least know what to
// type: a stale ID, a typo, a session that aged out. The store distinguishes
// "malformed" from "not found", which is the hard half, but neither message
// says where the real IDs are — and the store cannot say it, since it does not
// know it is being driven from a CLI.
func TestResolveBackgroundJob_PointsAtTheCommandThatListsIDs(t *testing.T) {
	store, err := backgroundstore.NewStoreAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	for _, query := range []string{
		"abc123",   // well-formed, no such job
		"nope-123", // not even hex
	} {
		_, err := resolveBackgroundJob(store, query)
		if err == nil {
			t.Fatalf("%q resolved against an empty store", query)
		}
		if !strings.Contains(err.Error(), "gokin agents") {
			t.Errorf("%q: error leaves the user with nowhere to look: %v", query, err)
		}
		// The store's own diagnosis must survive the wrapping — "malformed"
		// and "not found" are different problems and it already tells them
		// apart.
		if !strings.Contains(err.Error(), query) {
			t.Errorf("%q: wrapping lost the store's message: %v", query, err)
		}
	}
}

// Wrapping must not hide the cause from errors.Is/As for any caller that
// inspects it.
func TestResolveBackgroundJob_KeepsTheUnderlyingError(t *testing.T) {
	store, err := backgroundstore.NewStoreAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_, resolveErr := resolveBackgroundJob(store, "abc123")
	if resolveErr == nil {
		t.Fatal("expected an error")
	}
	if errors.Unwrap(resolveErr) == nil {
		t.Fatal("the hint replaced the cause instead of wrapping it")
	}
}
