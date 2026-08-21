// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package util

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/vishvananda/netlink"
)

// TestAwaitLinkByIndex_DeadlineCarriesLastError pins the #317
// diagnosability contract: the deadline error must carry the last
// attempt's underlying cause, or a persistent failure is
// indistinguishable from a startup race.
func TestAwaitLinkByIndex_DeadlineCarriesLastError(t *testing.T) {
	handle, err := netlink.NewHandle()
	if err != nil {
		t.Skipf("cannot open netlink handle in this environment: %v", err)
	}
	defer handle.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	// An index that cannot exist: kernel ifindexes are small positive
	// ints; 1<<30 will never appear during the 50ms budget.
	_, err = AwaitLinkByIndex(ctx, handle, 1<<30, 10*time.Millisecond)
	if err == nil {
		t.Fatal("expected an error for a link index that never appears")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error must wrap context.DeadlineExceeded, got: %v", err)
	}
	if !strings.Contains(err.Error(), "last attempt:") {
		t.Errorf("error must carry the last underlying cause, got: %v", err)
	}
}
