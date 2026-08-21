package context

import (
	"context"
	"sync"
	"testing"
	"time"

	"google.golang.org/genai"

	"gokin/internal/client"
	"gokin/internal/testkit"
)

// blockingScoreClient parks in SendMessage until its context is done, standing
// in for a provider that has stopped answering mid-compaction.
type blockingScoreClient struct {
	*testkit.MockClient
	entered chan struct{}
	once    sync.Once
}

func (c *blockingScoreClient) SendMessage(ctx context.Context, message string) (*client.StreamingResponse, error) {
	c.once.Do(func() { close(c.entered) })
	<-ctx.Done()
	return nil, ctx.Err()
}

func scoringMessages(n int) []*genai.Content {
	messages := make([]*genai.Content, 0, n)
	for i := 0; i < n; i++ {
		role := genai.Role(genai.RoleUser)
		if i%2 == 1 {
			role = genai.RoleModel
		}
		messages = append(messages, genai.NewContentFromText("a turn of conversation worth scoring", role))
	}
	return messages
}

// TestCreateSummaryPlanWithContextCarriesCancellation pins the reason the plan
// builder takes a context at all. Importance scoring can call the MODEL, and it
// used to do so on context.Background(): a compaction the caller had bounded at
// its 60s budget could sit here for the full model-round cap instead, and go on
// running after the user pressed Esc, still spending quota on a turn that no
// longer exists.
func TestCreateSummaryPlanWithContextCarriesCancellation(t *testing.T) {
	cl := &blockingScoreClient{MockClient: testkit.NewMockClient(), entered: make(chan struct{})}
	scorer := NewMessageScorer()
	scorer.SetSemanticClient(cl)
	// Well above the deadline the caller supplies, so only the caller's context
	// can be what ends the call.
	scorer.SetSemanticTimeout(30 * time.Second)

	strategy := DefaultSummaryStrategy()
	strategy.UseImportanceScoring = true
	// KeepStart(4) + RecentMessageCount(20) must still leave a middle of >=6
	// for semantic scoring to engage at all.
	strategy.MinMessagesForSummary = 2

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	done := make(chan *SummaryPlan, 1)
	go func() { done <- CreateSummaryPlanWithContext(ctx, scoringMessages(40), strategy, scorer) }()

	select {
	case <-cl.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("semantic scoring never started — the test is not exercising the model path")
	}

	select {
	case plan := <-done:
		if plan == nil {
			t.Fatal("plan must still be produced from heuristic scores after the model call is cut off")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the plan outlived its caller's deadline — scoring is not running on the caller's context")
	}
}
