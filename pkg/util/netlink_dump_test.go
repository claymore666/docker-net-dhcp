// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package util

import (
	"errors"
	"fmt"
	"testing"

	"github.com/vishvananda/netlink"
)

// TestDumpResult_SentinelKeepsTheResults is the direction #802 is
// about: netlink v1.3.1 hands back a usable result set alongside
// ErrDumpInterrupted, and the harness threw both away.
func TestDumpResult_SentinelKeepsTheResults(t *testing.T) {
	got, err := DumpResult([]int{1, 2, 3}, netlink.ErrDumpInterrupted)
	if err != nil {
		t.Fatalf("the sentinel was returned as an error: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("results = %v, want the three the dump produced", got)
	}
}

// TestDumpResult_WrappedSentinelKeepsTheResults is the same direction
// through errors.Is rather than through equality. A caller that wrapped
// the dump's error before it reached here still has a usable result set,
// and an implementation comparing with == would say otherwise.
func TestDumpResult_WrappedSentinelKeepsTheResults(t *testing.T) {
	wrapped := fmt.Errorf("LinkList: %w", netlink.ErrDumpInterrupted)
	got, err := DumpResult([]int{1}, wrapped)
	if err != nil {
		t.Fatalf("a wrapped sentinel was returned as an error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("results = %v, want the one the dump produced", got)
	}
}

// TestDumpResult_OtherErrorsAreRefused is the OTHER direction, and it is
// the one a helper written to make a red lane green would lose: EPERM,
// a closed socket and a parse failure must still be errors, and the
// results must not be handed on as though the dump had succeeded.
func TestDumpResult_OtherErrorsAreRefused(t *testing.T) {
	boom := errors.New("operation not permitted")

	got, err := DumpResult([]int{1, 2, 3}, boom)
	if !errors.Is(err, boom) {
		t.Fatalf("error = %v, want the caller's own error", err)
	}
	if got != nil {
		t.Errorf("results = %v, want nil: a failed dump has no result set to offer", got)
	}
}

// TestDumpResult_NoErrorIsUnchanged is the preservation control: the
// ordinary successful dump must behave exactly as the bare call did.
func TestDumpResult_NoErrorIsUnchanged(t *testing.T) {
	got, err := DumpResult([]string{"a", "b"}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("results = %v, want the input unchanged", got)
	}
}

// TestDumpResult_AcceptsARealDumpCall is the shape every call site
// uses, compiled rather than described: the dump's two results are the
// helper's two parameters, so a signature change that broke `f(g())`
// would fail here rather than at every migrated site at once.
func TestDumpResult_AcceptsARealDumpCall(t *testing.T) {
	links, err := DumpResult(netlink.LinkList())
	if err != nil {
		t.Fatalf("LinkList through the helper: %v", err)
	}
	if len(links) == 0 {
		t.Error("no links at all in this namespace; loopback alone should be one")
	}
}
