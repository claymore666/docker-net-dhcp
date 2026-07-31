package util

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/container"
)

// awaitStub counts calls so a test can tell "gave up immediately" from
// "polled until the deadline" — which is the entire behaviour change.
type awaitStub struct {
	calls int
	err   error
}

func (s *awaitStub) ContainerInspect(context.Context, string) (container.InspectResponse, error) {
	s.calls++
	return container.InspectResponse{}, s.err
}

func TestAwaitContainerInspect_NotFoundIsTerminal(t *testing.T) {
	// Verbatim shape of what the daemon returns for a removed container.
	stub := &awaitStub{err: fmt.Errorf(
		"Error response from daemon: No such container: deadbeef: %w", cerrdefs.ErrNotFound)}

	// A budget far longer than the test could tolerate if it were used:
	// the point is that it is not used.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	start := time.Now()
	_, err := AwaitContainerInspect(ctx, stub, "deadbeef", 10*time.Millisecond)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected an error for a container that does not exist")
	}
	if stub.calls != 1 {
		t.Errorf("asked the daemon %d times about a container it says does not exist; want 1 — "+
			"polling an absence can only end in the deadline", stub.calls)
	}
	if elapsed > time.Second {
		t.Errorf("took %v to give up on a removed container", elapsed)
	}
	// The chain has to survive, or callers cannot tell this apart from a
	// slow daemon — which is the misclassification the change exists to
	// end (#401).
	if !cerrdefs.IsNotFound(err) {
		t.Errorf("NotFound did not survive in the returned error: %v", err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Error("reported as a deadline; it is an absence")
	}
}

func TestAwaitContainerInspect_RetriesOtherErrorsAndKeepsTheLastOne(t *testing.T) {
	stub := &awaitStub{err: errors.New("connection refused")}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()

	_, err := AwaitContainerInspect(ctx, stub, "deadbeef", 20*time.Millisecond)

	if err == nil {
		t.Fatal("expected a deadline error")
	}
	if stub.calls < 2 {
		t.Errorf("gave up after %d call(s); a transient error must still be retried", stub.calls)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("want a deadline error, got %v", err)
	}
	// The sibling helpers have always reported the last attempt.
	// Discarding it here is why a Join timeout said only "context
	// deadline exceeded" while the same failure elsewhere named a
	// missing file.
	if !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("the last attempt's error was discarded: %v", err)
	}
}
