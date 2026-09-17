// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import (
	"errors"
	"fmt"

	"github.com/claymore666/dhcp-golib/lease"
	"github.com/claymore666/dhcp-golib/proto"
)

// A DHCPv6 acquisition that produced no address has more than one cause
// and the plugin has to tell them apart (#816).
//
// BEFORE THIS, IT COULD NOT. Every ending but the stateless Reply
// arrived as one sentence naming a proto.Reason, and the plugin's
// verdict was drawn from the router advertisement alone -- so a server
// that ANSWERED and said NoAddrsAvail and a server that never answered
// produced the same counter, the same message and the same advice. RFC
// 9915 section 18.2.10 is the client's licence to report the
// difference: "The client MAY choose to report any status code or
// message from the Status Code option in the Reply message."
//
// THE CODE CROSSES THIS PACKAGE AS A STRING AND NOT AS A wire TYPE.
// pkg/plugin names no library type (M6b, D22/D23); what it needs is a
// verdict and a name to print, and V6RefusalStatus hands it both.

// v6Refused is a DHCPv6 server that answered this client and refused
// it: RFC 9915 section 21.13's Status Code option carrying something
// other than Success.
type v6Refused struct {
	// status is the code's own name as the library prints it --
	// "NoAddrsAvail", "NotOnLink", or `status(N)` for a code IANA
	// registered after this build. It is a name and never a number,
	// because the number is meaningless in a log line and the library
	// already owns the mapping.
	status string
	note   string
}

func (e *v6Refused) Error() string {
	if e.note == "" {
		return fmt.Sprintf("dhcp: the DHCPv6 server refused this client: %s", e.status)
	}
	return fmt.Sprintf("dhcp: the DHCPv6 server refused this client: %s (%s)", e.status, e.note)
}

// ErrNoSLAACPrefix is a router that WAS heard and advertised no prefix
// this client could form an address from: no Prefix Information option
// with the Autonomous flag, or only ones RFC 4862 section 5.5.3
// refuses.
//
// It is its own error for the reason ErrNoDHCPv6OnSegment is: the thing
// to go and fix is different. "No router on this link" is a missing
// router, "the router advertises no autonomous prefix" is a router
// configured for a mode this network did not ask for, and one message
// for both is the confusion the v6 verdicts exist to end.
var ErrNoSLAACPrefix = errors.New("dhcp: a router advertises on this link and none of its prefixes " +
	"formed an address (RFC 4862 section 5.5.3)")

// ErrNoV6Server is a segment whose DHCPv6 server answered nothing at
// all, reported by the machine rather than by a deadline.
//
// MEASURED, library v1.0.0: no DHCPv6 path produces this reason today.
// A Solicit that nobody answers retransmits under RFC 9915 section
// 18.2.1's schedule, which has no retransmission count and no duration
// bound, so silence ends at the caller's acquisition budget and arrives
// as that budget's error -- including under `ipv6_auto_strict`, where
// the machine's own journal says "no fallback is configured: a silent
// server ends this acquisition" (proto/machine6_slaac.go:433) and then
// keeps soliciting. The reason is v4's (proto/machine.go:349) and is
// mapped here so that a v6 path added to the library later arrives as
// the right verdict on its first run rather than as an unclassified
// failure.
//
// The plugin's "the server is there and said nothing" verdict is
// therefore NOT drawn from this error; it is drawn from the router
// advertisement's managed flag, which is the observation that has
// always decided it (classifyV6Absence).
var ErrNoV6Server = errors.New("dhcp: no DHCPv6 server answered on this segment")

// ErrNoV6Router is router discovery that ended with no Router
// Advertisement at all: RFC 4861 section 6.3.7's schedule ran out in a
// mode whose address can only come from an advertisement.
//
// It is NOT ErrNoV6Server, and the library's own comment on the two
// reasons says why: "A link with no router and a link whose DHCPv6
// server is silent are different links" (proto/action.go:466). The
// plugin already tells this one apart from the router observation it
// takes at the deadline, and this error exists so that the sentence the
// operator reads names the right missing thing.
var ErrNoV6Router = errors.New("dhcp: no IPv6 router advertises on this segment")

// v6FailureCause turns a lease.Failed event into the cause the plugin's
// verdict is drawn from, or nil when this failure is not one of the
// three the verdict table names.
//
// nil AND NOT A CATCH-ALL. The caller keeps its own sentence for every
// other reason, so a reason added to the library later arrives as a
// reason-named error rather than as one of these three by default --
// which is the direction that matters, because each of these carries a
// counter and a documented operator action.
func v6FailureCause(ev lease.Event) error {
	switch ev.Reason {
	case proto.ReasonNak:
		// §21.13 makes an absent Status Code and Success one verdict,
		// and neither refuses anybody. A Nak with the zero code is a
		// v4-shaped refusal that has no code to carry and cannot reach
		// a v6 event; it is checked rather than assumed, because the
		// alternative is a refusal message that names "Success".
		if ev.Status == 0 {
			return nil
		}
		return V6Refusal(ev.Status.String(), ev.Note)
	case proto.ReasonNoPrefix:
		return fmt.Errorf("%w: %v", ErrNoSLAACPrefix, ev.Note)
	case proto.ReasonNoServer:
		return fmt.Errorf("%w: %v", ErrNoV6Server, ev.Note)
	case proto.ReasonNoRouter:
		// THIS DOES NOT DECIDE THE VERDICT and it is not meant to. The
		// plugin reads "was a router heard" from the observation it
		// takes at the deadline, which covers every mode; this arm only
		// makes the sentence name the router rather than the server.
		// Two derivations of one fact would be the shape where the
		// looser one decides.
		return fmt.Errorf("%w: %v", ErrNoV6Router, ev.Note)
	}
	return nil
}

// V6Refusal is the cause a DHCPv6 server's refusal is reported as.
//
// It is the chassis's own constructor, exported because the verdict
// drawn from it lives in pkg/plugin: that package decides what a
// refusal MEANS for an endpoint, and a test of that decision has to be
// able to hand it one. The alternative was for the caller to build a
// lease.Event, which would put a library type in a package whose whole
// rule is that it names none.
//
// status is the code's NAME as the library prints it. Nothing here
// parses it and nothing branches on it; it is carried to the operator's
// log line and to V6RefusalStatus and nowhere else.
func V6Refusal(status, note string) error {
	return &v6Refused{status: status, note: note}
}

// V6RefusalStatus reports whether this error is a DHCPv6 server's
// refusal, and the status code's name if it is.
//
// The name is returned rather than the code so that the caller can put
// it in a message and in nothing else. A caller that could branch on
// the number would be a second place deciding what a status code means,
// and there is exactly one: the library.
func V6RefusalStatus(err error) (string, bool) {
	var r *v6Refused
	if errors.As(err, &r) {
		return r.status, true
	}
	return "", false
}
