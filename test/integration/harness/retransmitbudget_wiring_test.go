// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

// IPAcquisitionBudget lives behind the integration tag, so the one
// check that compares the two budgets lives behind it too.

//go:build integration

package harness

import (
	"testing"
)

// TestRetransmitBudget_TwoLossesFitTheBudgetTheFirstExchangeHas keeps
// the two waits in the same story: the wait a cell gives the bound
// event stays inside the one RunContainer has given the first exchange
// since the suite began. A budget that outgrew it would mean cells
// waiting past the point the container start itself would have failed.
func TestRetransmitBudget_TwoLossesFitTheBudgetTheFirstExchangeHas(t *testing.T) {
	if got := RetransmitBudget(2); got > IPAcquisitionBudget {
		t.Errorf("RetransmitBudget(2) = %v, past the %v a container start is given",
			got, IPAcquisitionBudget)
	}
}
