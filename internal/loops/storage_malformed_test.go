package loops

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestFileStorage_ParseableButNotALoopIsRefused covers the half that
// TestFileStorage_CorruptFileSkipped leaves open. That test writes "not json",
// which fails at the decoder. These files all decode successfully and are
// still not loop state: JSON `null`, an empty object, an array, a document
// whose fields have the wrong types. Two independent layers stand between them
// and the scheduler — Loop.Validate, and Load's insistence that the filename
// agree with the loop's own ID — and this pins their combined outcome, because
// either one alone still holds when the other is removed. Disable both and a
// zero-valued Loop arrives with an empty ID and an empty mode: a phantom entry
// the user never created and cannot remove by name. Refusal must also name the
// file, since the alternative is a loop that silently stops existing.
func TestFileStorage_ParseableButNotALoopIsRefused(t *testing.T) {
	dir := t.TempDir()
	s := NewFileStorage(dir)
	good := &Loop{
		ID: "loop-good", Task: "keep me running", Mode: ModeInterval,
		IntervalSeconds: 300, Status: StatusRunning, CreatedAt: time.Now(),
	}
	if err := s.Save(good); err != nil {
		t.Fatal(err)
	}

	decodesButIsNotALoop := map[string]string{
		"null.json":        "null",
		"empty_obj.json":   "{}",
		"array.json":       "[1,2,3]",
		"wrong_types.json": `{"id":123,"task":false}`,
	}
	for name, body := range decodesButIsNotALoop {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	// A file that is not loop state at all must be ignored, not reported as
	// broken loop state.
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("scratch"), 0600); err != nil {
		t.Fatal(err)
	}

	loaded, errs := s.Load()

	if len(loaded) != 1 || loaded[0].ID != good.ID {
		ids := make([]string, 0, len(loaded))
		for _, l := range loaded {
			ids = append(ids, l.ID+"/"+string(l.Mode))
		}
		t.Fatalf("only the real loop may load; a zero-valued Loop reaching the "+
			"scheduler is a phantom the user cannot remove. loaded=%v", ids)
	}
	for name := range decodesButIsNotALoop {
		found := false
		for _, err := range errs {
			if strings.Contains(err.Error(), name) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%s was skipped without saying so; errors=%v", name, errs)
		}
	}
	for _, err := range errs {
		if strings.Contains(err.Error(), "notes.txt") {
			t.Errorf("a non-.json file is not loop state and must not be reported: %v", err)
		}
	}
}
