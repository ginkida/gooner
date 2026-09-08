package memory

import (
	"fmt"
	"testing"
)

// The store is bounded, which is right — it is loaded into every session's
// prompt. What was wrong is that reaching the bound was invisible to the
// caller: the write was dropped and nothing said so, so the tool above it
// reported the fact as memorized. From then on the memory is write-only and
// keeps saying yes.
func TestSetPreference_ReportsRefusalAtTheLimit(t *testing.T) {
	pl, err := NewProjectLearning(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	for i := range maxProjectLearningPreferences {
		if !pl.SetPreference(fmt.Sprintf("fact:key-%05d", i), "value") {
			t.Fatalf("refused entry %d while the store still had room", i)
		}
	}

	if pl.SetPreference("fact:one-too-many", "value") {
		t.Fatal("claimed to store an entry past the limit")
	}
	// An existing key must still be updatable at the limit — correcting a
	// stored fact adds nothing, and refusing it would make a full store
	// impossible to fix.
	if !pl.SetPreference("fact:key-00000", "corrected value") {
		t.Fatal("refused to update an entry that already exists, so a full store could never be corrected")
	}
}

// Patterns were the suspected second door and are not one: the store appends,
// then re-sorts with LastUsed descending as the primary key, and LearnPattern
// stamps that to now — so the new pattern always survives and the least
// recently used is what goes. This pins the property, because the reasoning
// that made it look like a defect (truncation sorts by name) reads correctly
// off the last line of the comparator.
func TestLearnPattern_EvictsTheLeastRecentlyUsedNotTheNewEntry(t *testing.T) {
	pl, err := NewProjectLearning(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	for i := range maxProjectLearningPatterns {
		if !pl.LearnPattern(fmt.Sprintf("aaa-%05d", i), "desc", nil, nil) {
			t.Fatalf("refused pattern %d while the store still had room", i)
		}
	}

	// Sorts last by name, so it would be the casualty if name were the key.
	if !pl.LearnPattern("zzz-newest", "desc", nil, nil) {
		t.Fatal("dropped the newest pattern; eviction must take the least recently used")
	}
	// The oldest entry is what left; re-learning it must be accepted again.
	if !pl.LearnPattern("aaa-00000", "desc", nil, nil) {
		t.Fatal("could not re-learn an evicted pattern")
	}
}

// An empty name is the one refusal on this path, and it must be visible for
// the same reason: a caller told nothing assumes it worked.
func TestLearnPattern_ReportsAnEmptyName(t *testing.T) {
	pl, err := NewProjectLearning(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if pl.LearnPattern("", "desc", nil, nil) {
		t.Fatal("claimed to store a pattern with no name")
	}
}
