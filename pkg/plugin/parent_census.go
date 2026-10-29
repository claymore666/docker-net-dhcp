// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/vishvananda/netlink"
)

// pendingEndpoint is a CreateEndpoint call in flight: its network and when it started.
type pendingEndpoint struct {
	network string
	since   time.Time
}

// censusListCap bounds the children an EBUSY error lists, since the daemon shows the whole text to the user.
const censusListCap = 5

type heldChild struct {
	endpoint, network, kind string
	where                   string
	since                   time.Time
	recovered               bool
}

// leftChild is an endpoint Leave stopped managing, whose child can sit in a dying sandbox until DeleteEndpoint (#1372).
type leftChild struct {
	parent string
	child  heldChild
	at     time.Time
}

func managerChild(m *dhcpManager, kind string) heldChild {
	return heldChild{endpoint: m.joinReq.EndpointID, network: m.joinReq.NetworkID, kind: kind, since: m.started,
		recovered: m.recovered}
}

// markEndpointLeft keeps a leaving macvlan or ipvlan endpoint on its fingerprint for the census (#1372).
func (p *Plugin) markEndpointLeft(endpointID string, m *dhcpManager) {
	kind := m.opts.effectiveMode()
	if kind != ModeMacvlan && kind != ModeIPvlan {
		return
	}
	left := &leftChild{parent: m.opts.linkParent(), child: managerChild(m, kind), at: time.Now()}
	p.mu.Lock()
	defer p.mu.Unlock()
	if fp, ok := p.endpointFingerprints[endpointID]; ok {
		fp.left = left
		p.endpointFingerprints[endpointID] = fp
	}
}

func (c heldChild) String() string {
	var parts []string
	if c.network != "" {
		parts = append(parts, "network "+shortID(c.network))
	}
	parts = append(parts, c.kind, c.where)
	age := time.Since(c.since).Round(time.Second).String()
	switch {
	case c.since.IsZero():
	case c.recovered:
		parts = append(parts, "recovered by the plugin restart "+age+" ago")
	default:
		parts = append(parts, "age "+age)
	}
	return fmt.Sprintf("endpoint %v (%v)", shortID(c.endpoint), strings.Join(parts, ", "))
}

// parentCensus is what the plugin can attribute among a parent's children when the kernel refuses another one.
type parentCensus struct {
	held                 []heldChild
	unheld, unheldTagged int
}

// census attributes parent's children: the plugin's endpoints on it, whose links sit in containers out of the host's
// link table, and the host-namespace children in links. It calls no daemon, since an EBUSY answers inside the daemon's
// ContainerStart (#406), and it reads no container ID while holding mu (#1189 holds ctrIDMu across a lookup).
func (p *Plugin) census(parent string, parentIndex int, links []netlink.Link) parentCensus {
	held := map[string]heldChild{}
	pending := map[string]pendingEndpoint{}
	p.creatingEndpoints.Range(func(k, v any) bool {
		id, _ := k.(string)
		pe, _ := v.(pendingEndpoint)
		pending[id] = pe
		return true
	})

	var managers []*dhcpManager
	leaving := map[string]leftChild{}
	p.mu.Lock()
	for _, m := range p.persistentDHCP {
		managers = append(managers, m)
	}
	for id, fp := range p.endpointFingerprints {
		if fp.left != nil && fp.left.parent == parent {
			leaving[id] = *fp.left
		}
	}
	p.mu.Unlock()
	for _, m := range managers {
		kind := m.opts.effectiveMode()
		if m.opts.linkParent() != parent || (kind != ModeMacvlan && kind != ModeIPvlan) {
			continue
		}
		where := "in a container"
		if id := m.cachedContainerID(); id != "" {
			where = "container " + shortID(id)
		} else if m.joinReq.SandboxKey != "" {
			where = "sandbox " + shortID(filepath.Base(m.joinReq.SandboxKey))
		}
		h := managerChild(m, kind)
		h.where = where
		held[m.joinReq.EndpointID] = h
	}
	for id, l := range leaving {
		if _, ok := held[id]; !ok {
			l.child.where = "left its container " + time.Since(l.at).Round(time.Second).String() + " ago"
			held[id] = l.child
		}
	}

	// The IPAM endpoint path leaves its child untagged, so a name match on an in-flight or managed endpoint counts.
	byName := map[string]string{}
	for id := range pending {
		byName[subLinkName(id)] = id
	}
	for id := range held {
		byName[subLinkName(id)] = id
	}

	var c parentCensus
	for _, l := range links {
		a := l.Attrs()
		if a.ParentIndex != parentIndex {
			continue
		}
		id, tagged := strings.CutPrefix(a.Alias, endpointAliasPrefix)
		if !tagged {
			id = byName[a.Name]
		}
		if id == "" || !p.endpointKnown(id) {
			c.unheld++
			if tagged {
				c.unheldTagged++
			}
			continue
		}
		if _, ok := held[id]; ok {
			continue
		}
		pe := pending[id]
		held[id] = heldChild{endpoint: id, network: pe.network, kind: l.Type(), where: "host link " + a.Name, since: pe.since}
	}

	for _, h := range held {
		c.held = append(c.held, h)
	}
	sort.Slice(c.held, func(i, j int) bool { return c.held[i].endpoint < c.held[j].endpoint })
	return c
}

func (c parentCensus) String(parent string) string {
	var b strings.Builder
	if len(c.held) == 0 {
		fmt.Fprintf(&b, "this plugin holds no child on %q", parent)
	} else {
		var listed []string
		for i, h := range c.held {
			if i == censusListCap {
				listed = append(listed, fmt.Sprintf("and %d more", len(c.held)-censusListCap))
				break
			}
			listed = append(listed, h.String())
		}
		fmt.Fprintf(&b, "this plugin holds %d %v on %q: %v", len(c.held), childNoun(len(c.held)), parent, strings.Join(listed, "; "))
	}
	fmt.Fprintf(&b, "; %d other %v on it in the host namespace", c.unheld, childNoun(c.unheld))
	if c.unheldTagged > 0 {
		fmt.Fprintf(&b, ", %d of them tagged by this plugin for an endpoint it no longer holds", c.unheldTagged)
	}
	return b.String()
}

func childNoun(n int) string {
	if n == 1 {
		return "child"
	}
	return "children"
}

// warn logs the census beside the error, so the plugin's log tells a leftover from a foreign holder (#1372).
func (c parentCensus) warn(parent, mode, text string) {
	log.WithFields(log.Fields{
		"parent":        parent,
		"mode":          mode,
		"held":          len(c.held),
		"unheld":        c.unheld,
		"unheld_tagged": c.unheldTagged,
	}).Warn("The parent refused a child with EBUSY: " + text)
}
