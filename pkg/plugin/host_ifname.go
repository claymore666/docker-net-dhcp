// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"errors"
	"fmt"
	"strings"
	"sync"

	log "github.com/sirupsen/logrus"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
	"github.com/claymore666/docker-net-dhcp/v2/pkg/util"
)

// The values of the `host_ifname` network option (#978).
const (
	// HostIfnameOff leaves the host-side veth named after the endpoint.
	HostIfnameOff = ""
	// HostIfnameContainerName names it after the container, as `docker ps` prints it.
	HostIfnameContainerName = "container_name"
	// HostIfnameHostname names it after the container's hostname, which is not unique on a host.
	HostIfnameHostname = "hostname"
)

// hostIfnameMaxLen is IFNAMSIZ-1: measured on 6.12.107, 15 bytes are taken and 16 are refused with ERANGE (#978).
const hostIfnameMaxLen = 15

// hostIfnameSuffixLen: five hex characters of the endpoint ID, the start of the `dh-` name it replaces (#978).
const hostIfnameSuffixLen = 5

func parseHostIfname(v string) (string, error) {
	switch v {
	case HostIfnameOff, HostIfnameContainerName, HostIfnameHostname:
		return v, nil
	default:
		return "", fmt.Errorf("%w: host_ifname %q is not one of %s, %s",
			util.ErrIPAM, v, HostIfnameContainerName, HostIfnameHostname)
	}
}

// hostIfnameSource drops a hostname safeHostname refused, since deriveHostIfname would make it a legal name (#978).
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

// deriveHostIfname applies the rule docs/reference.md states, or returns "" (#978):
//  1. every byte outside the name rule becomes '-';
//  2. leading non-alphanumeric bytes are dropped;
//  3. over hostIfnameMaxLen the name keeps its first bytes and ends in '-' plus the endpoint ID's first hostIfnameSuffixLen;
//  4. the result is checked against dhcp.ValidIfaceName.
//
// The suffix is keyed on the endpoint, since two containers may share a hostname.
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

// afterAttach runs the lookup, then the rename, which reads the name the lookup filled (#961, #978). Both values are
// read after lookup runs: on engine 29.8.1 the netns-key route inspects only here, so a value copied at the call is
// empty (#1051).
func (m *dhcpManager) afterAttach(phases *joinPhases, inspected bool, lookup func() error, ctrName, ctrHostname *string) {
	if !inspected {
		inspected = m.nameTheRunningClient(phases, lookup, ctrHostname)
	}
	m.renameHostLink(inspected, *ctrName, *ctrHostname)
}

// hostLinkNaming closes the rename window to this process's lookups of the generated name (#1051). Measured on
// 6.12.107 in a user namespace: no state has both names resolving, since an altname equal to the current name and a
// rename onto the link's own altname are both EEXIST. A miss in DeleteEndpoint returns nil and strands the veth.
// Write-held across the rename and altname, read-held for one lookup; outside readers must wait the window out.
var hostLinkNaming sync.RWMutex

// hostLinkByGeneratedName looks a host-side veth up by its generated name, through the guard (#1051).
func hostLinkByGeneratedName(name string) (netlink.Link, error) {
	hostLinkNaming.RLock()
	defer hostLinkNaming.RUnlock()
	return nlLinkByName(name)
}

// renameHostLink renames this endpoint's host veth after the attach, since the container name is not known at
// CreateEndpoint (moby/moby#52871, #978). The old name stays as an altname because four sites look it up and
// DeleteEndpoint treats a miss as a finished teardown. Bridge mode only: macvlan and ipvlan children leave the host.
// `inspected` is a parameter so the PID-fallback and register_dns routes are renamed too.
func (m *dhcpManager) renameHostLink(inspected bool, ctrName, ctrHostname string) {
	if !inspected || m.opts.HostIfname == HostIfnameOff || m.opts.effectiveMode() != ModeBridge {
		return
	}
	// Held from here: hostLinkByGeneratedName takes the read lock this goroutine is about to hold for writing (#1051).
	hostLinkNaming.Lock()
	defer hostLinkNaming.Unlock()

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

	// Recovery calls Start again on already renamed links; measured on 6.12.107, redoing the path fails with EEXIST.
	// The name is read back from the kernel, since hostName resolves through the altname (#978).
	if link.Attrs().Name == want {
		m.plugin.hostIfnamesApplied.Add(1)
		return
	}

	// A lookup that resolved through the altname means it is already on the link, and re-adding it is EEXIST: the
	// `docker rename` then restart case. Taken before the rename, from the lookup, not from Attrs().AltNames (#978,
	// #1051).
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

	// Every existing lookup of the old name resolves through the altname, so a failure here undoes the rename (#978).
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
