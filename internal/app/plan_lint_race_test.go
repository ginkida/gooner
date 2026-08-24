package app

import (
	"context"
	"sync"
	"testing"

	"gokin/internal/config"
	"gokin/internal/donegate"
	"gokin/internal/plan"
)

// TestPlanLintReaders_NoRaceWithApplyConfigSwap extends the ApplyConfig-reader
// class to the plan lint path. lintPlanBeforeApproval is registered as the plan
// manager's lint handler and runs wherever exit_plan_mode was called — and that
// tool is in the sub-agent tool set, including the workspace-isolation lists —
// so it can execute on a SUB-AGENT's goroutine while the foreground swaps
// a.config. It read a.config.Plan.RequireExpectedArtifactPaths once per step,
// and the validateVerifyCommandSafety it calls read a.config.Plan.VerifyPolicy
// per command, both without the lock. Under -race this fails loudly if a future
// edit drops the snapshot.
func TestPlanLintReaders_NoRaceWithApplyConfigSwap(t *testing.T) {
	a := &App{config: config.DefaultConfig(), workDir: t.TempDir()}
	c1 := config.DefaultConfig()
	c2 := config.DefaultConfig()
	c2.Plan.RequireExpectedArtifactPaths = true

	p := &plan.Plan{
		ID:    "lint-race",
		Title: "race",
		Steps: []*plan.Step{
			{ID: 1, Title: "build", VerifyCommands: []string{"go build ./..."}},
			{ID: 2, Title: "test", VerifyCommands: []string{"go test ./..."}},
		},
	}
	profile := donegate.DetectProfile(a.workDir)

	var wg sync.WaitGroup
	wg.Add(2)
	// Reader #1: the lint handler, as a sub-agent's exit_plan_mode would run it.
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			_ = a.lintPlanBeforeApproval(context.Background(), p)
		}
	}()
	// Reader #2: the verify-command policy check it calls per command.
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			_, _ = a.validateVerifyCommandSafety(context.Background(), "go test ./...", profile)
		}
	}()

	// Writer: ApplyConfig swapping a.config under a.mu.
	for i := 0; i < 500; i++ {
		a.mu.Lock()
		if i%2 == 0 {
			a.config = c1
		} else {
			a.config = c2
		}
		a.mu.Unlock()
	}
	wg.Wait()
}
