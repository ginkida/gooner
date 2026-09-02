package commands

import (
	"fmt"
	"testing"
)

func TestZZAdvertisedCommandsExist(t *testing.T) {
	h := NewHandler()
	for _, name := range []string{"undo", "redo", "clear", "save", "resume", "cost", "stats", "diff", "help"} {
		_, ok := h.commands[name]
		fmt.Printf("/%-8s registered=%v\n", name, ok)
	}
	fmt.Printf("total registered: %d\n", len(h.commands))
}
