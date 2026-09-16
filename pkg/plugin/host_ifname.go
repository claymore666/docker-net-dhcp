// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"errors"
	"fmt"
	"strings"

	log "github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
	"github.com/claymore666/docker-net-dhcp/v2/pkg/util"
)

// The values of the `host_ifname` network option (#978). Empty is
// HostIfnameOff, which is the `dh-<12 hex>` name every release before
// v2.2.0 used.
const (
	// HostIfnameOff leaves the host-side veth named after the endpoint.
	HostIfnameOff = ""
	// HostIfnameContainerName names it after the container, the name
	// `docker ps` prints and a compose file writes.
	HostIfnameContainerName = "container_name"
	// HostIfnameHostname names it after the container's hostname
	// (`docker run --hostname`), which defaults to the short container
	// ID and is NOT unique on a host.
	HostIfnameHostname = "hostname"
)

// hostIfnameMaxLen is IFNAMSIZ-1, the printable length the kernel
// accepts. MEASURED on 6.12.107: a 15-byte name is taken and a 16-byte
// one is refused with ERANGE, which is why the truncation below happens
// here and is never left to netlink.
const hostIfnameMaxLen = 15

// hostIfnameSuffixLen is how much of the endpoint ID a truncated name
// ends in. Five hex characters are the first five of the `dh-` name the
// derived one replaces, so a truncated name still points back at
// `docker network inspect`.
const hostIfnameSuffixLen = 5

// parseHostIfname normalises and validates the `host_ifname` option.
//
// The refusal happens once, at CreateNetwork, the same rule
// conflict_check and release_lease follow: a typo that silently selected
// the default would be a network whose operator believes its links are
// named after its containers and that names none of them.
func parseHostIfname(v string) (string, error) {
	switch v {
	case HostIfnameOff, HostIfnameContainerName, HostIfnameHostname:
		return v, nil
	default:
		return "", fmt.Errorf("%w: host_ifname %q is not one of %s, %s",
			util.ErrIPAM, v, HostIfnameContainerName, HostIfnameHostname)
	}
}

// hostIfnameSource is the string this network wants its host-side links
// named after, for one container. Empty when the option is off.
//
// ctrName is the Docker API's own Name field, which carries a leading
// slash that no interface name may contain.
//
// A HOSTNAME THIS PLUGIN REFUSED TO SEND BUYS NOTHING HERE EITHER.
// safeHostname drops a --hostname carrying a control character rather
// than putting it on the wire, because a value the container chose is
// attacker-supplied; deriveHostIfname below would have turned the same
// value into a perfectly legal interface name and given it the link, so
// the refusal is repeated as a predicate. dhcp.SafeValue and not
// safeHostname itself: the counter and the log line belong to the one
// call that decides what goes on the wire, and a second call would count
// one container twice.
func (o DHCPNetworkOptions) hostIfnameSource(ctrName, ctrHostname string) string {
	switch o.HostIfname {
	case HostIfnameContainerName:
		return strings.TrimPrefix(ctrName, "/")
	case HostIfnameHostname:
		if !dhcp.SafeValue(ctrHostname) {
			return ""
		}
		return ctrHostname
	}
	return ""
}

// deriveHostIfname turns a container's name into a kernel-legal
// interface name, or "" when nothing legal is left of it.
//
// DETERMINISTIC AND STATED, because the operator has to be able to
// predict it: `docs/reference.md` carries this rule in words and the
// boundary cases have their own tests.
//
//  1. every byte outside the name rule becomes '-'
//  2. leading bytes that are not alphanumeric are dropped, because a
//     kernel-legal name starts with one
//  3. over hostIfnameMaxLen the name keeps its first bytes and ends in
//     '-' plus the endpoint ID's first hostIfnameSuffixLen, so two long
//     names sharing a prefix do not become one name
//  4. what comes out is checked against dhcp.ValidIfaceName rather than
//     trusted, so a rule change there cannot leak a name past this
//
// Step 3 keys the suffix on the ENDPOINT and not on the source string.
// A hash of the name would give one container one name for the life of
// the host, which sounds better and is worse: two containers may share a
// hostname, and their truncated names would then be equal and the second
// would lose its rename to a collision it could not see.
func deriveHostIfname(source, endpointID string) string {
	b := make([]byte, 0, len(source))
	for i := 0; i < len(source); i++ {
		c := source[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '.', c == '-', c == '_':
			b = append(b, c)
		default:
			b = append(b, '-')
		}
	}
	for len(b) > 0 && !isHostIfnameAlnum(b[0]) {
		b = b[1:]
	}
	name := string(b)
	if name == "" {
		return ""
	}
	if len(name) > hostIfnameMaxLen {
		suffix := endpointID
		if len(suffix) > hostIfnameSuffixLen {
			suffix = suffix[:hostIfnameSuffixLen]
		}
		if suffix == "" {
			name = name[:hostIfnameMaxLen]
		} else {
			name = name[:hostIfnameMaxLen-1-len(suffix)] + "-" + suffix
		}
	}
	if !dhcp.ValidIfaceName(name) {
		return ""
	}
	return name
}

func isHostIfnameAlnum(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// afterAttach is everything the attach does once the container is
// already leasing and the attach itself can no longer fail (#961, #978).
//
// THE ORDER IS THE POINT. The lookup fills ctrHostname, and the rename
// reads it, so a rename placed first derives its name from an empty
// string and every endpoint on a `hostname` network keeps its `dh-`
// name. Both steps live here, in one function a test can drive, because
// the wiring between them is the part neither step's own tests can see.
//
// The rename call is unconditional and its condition lives inside it:
// the two routes that already had the name skip nameTheRunningClient
// entirely, and a rename guarded beside that call would have skipped
// them too.
func (m *dhcpManager) afterAttach(phases *joinPhases, inspected bool, lookup func() error, ctrName string, ctrHostname *string) {
	if !inspected {
		inspected = m.nameTheRunningClient(phases, lookup, ctrHostname)
	}
	m.renameHostLink(inspected, ctrName, *ctrHostname)
}

// renameHostLink gives this endpoint's host-side veth the name its
// network asked for (#978).
//
// WHERE IT RUNS IS THE WHOLE DESIGN. The container's name is not known
// at CreateEndpoint -- libnetwork does not carry it and moby/moby#52871
// is open -- so the link is created as `dh-<12 hex>` and renamed here,
// at the point #961 already has the daemon's answer in hand. It makes no
// daemon call of its own, and it runs after the attach has succeeded, so
// nothing it does can fail one: a container with a lease and an ugly
// interface name is a working container.
//
// THE OLD NAME STAYS ON THE LINK AS AN ALTNAME, and that is not a
// nicety. Four sites re-derive `dh-<12 hex>` and look it up without ever
// reading a name back from the kernel, and one of them is DeleteEndpoint,
// which treats a miss as the normal end of a forced teardown and returns
// nil. A rename with no altname would leave the veth on the bridge for
// the life of the host, silently. The altname is what keeps every one of
// those call sites correct without any of them being edited, so no
// future call site can be forgotten either.
//
// Only bridge mode reaches here. macvlan and ipvlan children are moved
// into the container's namespace and nothing of theirs survives on the
// host, which is why CreateNetwork refuses the option in those modes
// rather than accepting it and doing nothing.
//
// THE GUARD IS THE PARAMETER, and that is deliberate. The rename must
// run wherever the daemon answered, which includes the two routes that
// took the name BEFORE the client started and never enter
// nameTheRunningClient at all -- the PID fallback and register_dns. A
// caller-side `if` beside that function would have missed both, silently
// and only on those hosts, so `inspected` is asked for here instead and
// the one call site passes it unconditionally.
func (m *dhcpManager) renameHostLink(inspected bool, ctrName, ctrHostname string) {
	if !inspected || m.opts.HostIfname == HostIfnameOff || m.opts.effectiveMode() != ModeBridge {
		return
	}
	hostName, _ := vethPairNames(m.joinReq.EndpointID)
	want := deriveHostIfname(m.opts.hostIfnameSource(ctrName, ctrHostname), m.joinReq.EndpointID)

	if want == "" {
		m.plugin.hostIfnameFailures.Add(1)
		log.WithFields(m.logFields(false)).
			WithField("host_ifname", m.opts.HostIfname).
			Warn("The container's name has no characters an interface name may carry; this endpoint's host-side link keeps its generated name")
		return
	}
	if want == hostName {
		m.plugin.hostIfnamesApplied.Add(1)
		return
	}

	link, err := nlLinkByName(hostName)
	if err != nil {
		m.plugin.hostIfnameFailures.Add(1)
		log.WithError(err).WithFields(m.logFields(false)).
			WithField("host_link", hostName).
			Warn("This endpoint's host-side link could not be found to rename; it keeps its generated name")
		return
	}

	// ALREADY DONE IS DONE, and this is a live path, not a defence.
	// Recovery calls Start again for every endpoint it rebuilds after a
	// plugin restart, on links this already renamed. Without this the
	// code walks the whole path over a healthy link and the kernel
	// answers, MEASURED on 6.12.107: the rename to the name it already
	// has succeeds, the altname it already has is EEXIST, and the undo
	// is EEXIST too, because the link's own altname holds that name.
	// That is a warn counter and this file's loudest Error, "must be
	// removed by hand", about a link teardown resolves perfectly well,
	// for every container on such a network on every plugin restart.
	//
	// The name is read back from the kernel because only the kernel
	// knows it: hostName is what the lookup was keyed on and resolves
	// through the altname, so it says nothing about what the link is
	// called.
	if link.Attrs().Name == want {
		m.plugin.hostIfnamesApplied.Add(1)
		return
	}

	// AND WHICH NAME THE LOOKUP RESOLVED THROUGH says whether the
	// generated name still has to be put back.
	//
	// The lookup above was keyed on hostName. If that is not what the
	// link is called, the only thing it can have resolved through is
	// the altname, so the altname is already on the link and adding it
	// again is EEXIST -- which would be counted a failure and answered
	// with an undo that is EEXIST as well.
	//
	// This is the same defect the read-back closes, in the case where
	// `want` MOVED between two passes instead of staying put: `docker
	// rename web api` on a running container, then a plugin restart.
	// The link ends up named `api`, keeping its altname, on its bridge
	// and entirely correct, and without this the operator is told to
	// remove it by hand.
	//
	// Derived from the lookup rather than read from Attrs().AltNames
	// deliberately: the derivation cannot be defeated by a library that
	// does not fill that field in, and a link that carries neither name
	// never reaches here at all -- the lookup fails first.
	//
	// Taken BEFORE the rename, and it has to stay there. netlink v1.3.1
	// leaves Attrs().Name alone when LinkSetName succeeds, so today the
	// same expression below would still answer correctly; that is the
	// library's business and not a property to hang the altname on. Read
	// here, the answer is about the link the lookup returned whatever
	// any version of the library does to the struct afterwards.
	altNameIsAlreadyOnTheLink := link.Attrs().Name != hostName

	if err := nlLinkSetName(link, want); err != nil {
		if errors.Is(err, unix.EEXIST) {
			m.plugin.hostIfnameConflicts.Add(1)
			log.WithFields(m.logFields(false)).
				WithField("host_link", hostName).
				WithField("wanted", want).
				Warn("Another interface on this host already has the name this container asked for; this endpoint's host-side link keeps its generated name")
			return
		}
		m.plugin.hostIfnameFailures.Add(1)
		log.WithError(err).WithFields(m.logFields(false)).
			WithField("host_link", hostName).
			WithField("wanted", want).
			Warn("The kernel refused to rename this endpoint's host-side link; it keeps its generated name")
		return
	}

	// The altname is what every existing lookup of the old name
	// resolves through. Without it the rename above is a link nothing
	// can find, so a failure here is undone rather than reported.
	if !altNameIsAlreadyOnTheLink {
		if err := nlLinkAddAltName(link, hostName); err != nil {
			m.plugin.hostIfnameFailures.Add(1)
			if back := nlLinkSetName(link, hostName); back != nil {
				log.WithError(back).WithFields(m.logFields(false)).
					WithField("host_link", hostName).
					WithField("named", want).
					Error("This endpoint's host-side link was renamed, the old name could not be kept on it as an altname, and it could not be renamed back; teardown will not find it and it must be removed by hand")
				return
			}
			log.WithError(err).WithFields(m.logFields(false)).
				WithField("host_link", hostName).
				WithField("wanted", want).
				Warn("The old name could not be kept on this endpoint's host-side link as an altname, so the rename was undone; it keeps its generated name")
			return
		}
	}

	m.plugin.hostIfnamesApplied.Add(1)
	log.WithFields(m.logFields(false)).
		WithField("host_link", hostName).
		WithField("named", want).
		Info("This endpoint's host-side link is named after its container; the generated name is on it as an altname, which is what every lookup of that name resolves through")
}
