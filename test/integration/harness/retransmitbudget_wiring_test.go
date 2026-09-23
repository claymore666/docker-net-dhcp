// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

// IPAcquisitionBudget is behind the integration tag, so this comparison is too.

//go:build integration

package harness

import (
	"testing"
)

func TestRetransmitBudget_TwoLossesFitTheBudgetTheFirstExchangeHas(t *testing.T) {
	if got := RetransmitBudget(2); got > IPAcquisitionBudget {
		t.Errorf("RetransmitBudget(2) = %v, past the %v a container start is given",
			got, IPAcquisitionBudget)
	}
}
