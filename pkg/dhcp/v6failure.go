// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import (
	"errors"
	"fmt"

	"github.com/claymore666/dhcp-golib/lease"
	"github.com/claymore666/dhcp-golib/proto"
)

// RFC 9915 section 18.2.10 lets the client report the Reply's Status Code, so a refusing server and a silent one get
// different verdicts (#816). The code crosses as a name, since pkg/plugin names no library type.

// v6Refused is a DHCPv6 server's refusal: an RFC 9915 section 21.13 Status Code other than Success.
type v6Refused struct {
	// status is the code's name as the library prints it, `status(N)` for a code registered after this build (#816).
	status string
	note   string
}

func (e *v6Refused) Error() string {
	if e.note == "" {
		return fmt.Sprintf("dhcp: the DHCPv6 server refused this client: %s", e.status)
	}
	return fmt.Sprintf("dhcp: the DHCPv6 server refused this client: %s (%s)", e.status, e.note)
}

// A router with no autonomous prefix is misconfigured for this mode; a missing router is another fix, so another error
// (#816).

// ErrNoSLAACPrefix is a heard router with no prefix to form an address from (RFC 4862 section 5.5.3).
var ErrNoSLAACPrefix = errors.New("dhcp: a router advertises on this link and none of its prefixes " +
	"formed an address (RFC 4862 section 5.5.3)")

// Library v1.0.0: no v6 path produces this reason, since RFC 9915 section 18.2.1's Solicit schedule has no bound and
// silence ends at the acquisition budget; it is mapped so a future path lands right. The "server silent" verdict comes
// from the advertisement's managed flag in classifyV6Absence (#816).

// ErrNoV6Server is a segment whose DHCPv6 server answered nothing, as the machine reports it.
var ErrNoV6Server = errors.New("dhcp: no DHCPv6 server answered on this segment")

// The library keeps a missing router and a silent server apart; the plugin's verdict still comes from its own router
// observation (#816).

// ErrNoV6Router is RFC 4861 section 6.3.7 router discovery that ended with no Router Advertisement.
var ErrNoV6Router = errors.New("dhcp: no IPv6 router advertises on this segment")

// Returns nil, not a catch-all, so a reason added later keeps its own sentence, not one of these counters
// (#816).

// v6FailureCause turns a lease.Failed event into the cause the plugin's verdict is drawn from, or nil.
func v6FailureCause(ev lease.Event) error {
	switch ev.Reason {
	case proto.ReasonNak:
		// RFC 9915 section 21.13 treats an absent Status Code as Success; a zero-code Nak is v4-shaped and must not
		// read as a refusal.
		if ev.Status == 0 {
			return nil
		}
		return V6Refusal(ev.Status.String(), ev.Note)
	case proto.ReasonNoPrefix:
		return fmt.Errorf("%w: %v", ErrNoSLAACPrefix, ev.Note)
	case proto.ReasonNoServer:
		return fmt.Errorf("%w: %v", ErrNoV6Server, ev.Note)
	case proto.ReasonNoRouter:
		// The verdict reads "was a router heard" from the deadline observation; this arm only names the right thing
		// (#816).
		return fmt.Errorf("%w: %v", ErrNoV6Router, ev.Note)
	}
	return nil
}

// Exported so a pkg/plugin test can hand the verdict a refusal without a library type (#816).

// V6Refusal is the cause a DHCPv6 server's refusal is reported as, status being the code's library name.
func V6Refusal(status, note string) error {
	return &v6Refused{status: status, note: note}
}

// V6RefusalStatus reports whether err is a DHCPv6 server's refusal and returns the status code's name.
func V6RefusalStatus(err error) (string, bool) {
	var r *v6Refused
	if errors.As(err, &r) {
		return r.status, true
	}
	return "", false
}
