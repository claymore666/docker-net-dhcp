// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

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
	if !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("the last attempt's error was discarded: %v", err)
	}
}

func TestAwaitContainerInspect_ReportsAttemptCount(t *testing.T) {
	t.Run("dead context on arrival reports one attempt", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		f := &countingInspector{err: errors.New("context deadline exceeded")}
		_, err := AwaitContainerInspect(ctx, f, "c", time.Millisecond)
		if err == nil {
			t.Fatal("want an error against a cancelled context")
		}
		if !strings.Contains(err.Error(), "1 attempts") {
			t.Errorf("error %q does not say how many attempts were made", err)
		}
	})

	t.Run("a repeatedly refusing daemon reports both ends", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
		defer cancel()
		f := &countingInspector{err: errors.New("connection refused")}
		_, err := AwaitContainerInspect(ctx, f, "c", time.Millisecond)
		if err == nil {
			t.Fatal("want an error once the budget runs out")
		}
		if !strings.Contains(err.Error(), "connection refused") {
			t.Errorf("error %q lost the actual failure", err)
		}
		if f.calls < 2 {
			t.Fatalf("only %d attempts; the retry loop did not run", f.calls)
		}
	})
}

type countingInspector struct {
	err   error
	calls int
}

func (c *countingInspector) ContainerInspect(_ context.Context, _ string) (container.InspectResponse, error) {
	c.calls++
	return container.InspectResponse{}, c.err
}
