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

func TestAwaitLinkByIndex_DeadlineCarriesLastError(t *testing.T) {
	handle, err := netlink.NewHandle()
	if err != nil {
		t.Skipf("cannot open netlink handle in this environment: %v", err)
	}
	defer handle.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	// Kernel ifindexes are small positive ints, so 1<<30 never appears.
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
