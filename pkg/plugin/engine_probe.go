// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"fmt"
	"time"

	log "github.com/sirupsen/logrus"
)

// engineProbeTimeout bounds the one startup probe. It matches the
// client's own per-call timeout (newDockerClient's WithTimeout): a
// longer budget here would only wait on a client that has already
// given up, and a shorter one would cut a call the client is still
// willing to make.
const engineProbeTimeout = 2 * time.Second

// unknownEngineField is what the health document carries when the probe
// did not reach the daemon. A WORD, never an empty string, for the
// reason `version` and `commit` already give in the reference: an empty
// value reads as "nothing to report" where this means "this process
// never found out".
const unknownEngineField = "unknown"

// WHICH VALUE THE FLOOR COMPARES, and why it is the engine version and
// not the negotiated API version.
//
// The floor is a property of the DAEMON BUILD, not of the API. What
// failed on the row below the floor was `docker plugin enable` — the
// plugin subsystem — and the plugin's own API calls (NetworkList,
// NetworkInspect, ContainerInspect, the ping) are older than every
// engine in the matrix. So an API-version floor would be a number
// nothing here measured, gating calls that all work far below it.
//
// The negotiated API version is published beside it and never refuses
// on its own, for two reasons that pull the same way:
//
//   - it is min(this client's maximum, the daemon's maximum), so on an
//     engine NEWER than the client library it reports the client's
//     number rather than the daemon's. Refusing on it would make a
//     library pin an engine requirement.
//   - an operator may put a socket proxy in front of the daemon
//     (DOCKER_HOST, docker_transport.go), and a proxy that pins an old
//     API says nothing about which engine is behind it.
//
// A negotiated API below the one the floor row reported is still worth
// saying out loud — it is the shape where the two numbers disagree —
// so it is a warning naming both, and the health document carries both
// for an operator to read. TestEngineFloor_APIBelowFloorWarnsWithoutRefusing
// drives it.

// MinEngineAPIVersion is the API version the floor row reported, and it
// is a MEASUREMENT, not a threshold: engine-matrix run 34594749584 saw
// 20.10.24 answer with API 1.41 and 19.03.15 with 1.40. Nothing refuses
// on it; see the block above.
const MinEngineAPIVersion = "1.41"

// probeEngine pings the daemon once, records what it said, and refuses
// below the floor.
//
// THE THREE OUTCOMES, kept distinct on purpose:
//
//   - the daemon answered with a version at or above the floor: the
//     identity is recorded and the plugin starts.
//   - the daemon answered with a version below the floor: an error,
//     fatal in main.go. The same shape as a lease record whose lock
//     another process holds — the condition does not improve on its
//     own, and starting means failing later somewhere an operator is
//     not looking.
//   - the daemon did not answer: NOT a refusal. Docker respawns this
//     plugin during its own startup and the daemon is routinely not
//     serving yet at this moment (#383, which is why recovery has a
//     deferred half at all). Refusing here would turn the normal
//     startup race into a plugin that will not install. The identity
//     reads `unknown`, a warning says the floor was not checked, and
//     reprobeEngine takes it again from the deferred recovery path,
//     which runs at the moment the daemon is known to be up.
func (p *Plugin) probeEngine(ctx context.Context) error {
	id, err := p.identifyEngine(ctx)
	if err != nil {
		p.engine.Store(&engineIdentity{Version: unknownEngineField, APIVersion: unknownEngineField})
		log.WithError(err).WithField("floor", MinEngineVersion).
			Warn("The Docker daemon did not answer at startup, so the engine version is unknown and the minimum was not checked")
		return nil
	}
	p.engine.Store(&id)
	return p.judgeEngine(id)
}

// reprobeEngine takes the identity again once the daemon is known to be
// answering. It runs only when the startup probe found no daemon.
//
// IT DOES NOT REFUSE, and the boundary is deliberate rather than
// forgotten. By this point the socket is up and the daemon may already
// have driven CreateNetwork through this process; tearing the process
// down from a goroutine at that moment replaces one visible failure
// with a less visible one. What it owes an operator is the statement,
// and that is what it makes: an ERROR naming the floor and the engine
// seen, plus the version in the health document, where before there was
// `unknown`.
func (p *Plugin) reprobeEngine(ctx context.Context) {
	if cur := p.engine.Load(); cur != nil && cur.Version != unknownEngineField {
		return
	}
	id, err := p.identifyEngine(ctx)
	if err != nil {
		log.WithError(err).Debug("engine: the daemon still does not answer a version query")
		return
	}
	p.engine.Store(&id)
	if err := p.judgeEngine(id); err != nil {
		log.WithError(err).Error("engine: this daemon is below the minimum engine version; the plugin is running unsupported")
	}
}

// judgeEngine is the whole verdict, in one place so the startup arm and
// the deferred arm cannot drift. It DETECTS; what a caller does with a
// below-floor verdict — refuse to start, or say so loudly because it is
// already serving — is the caller's, and is the only difference between
// the two arms.
func (p *Plugin) judgeEngine(id engineIdentity) error {
	fields := log.Fields{
		"engine_version": id.Version,
		"api_version":    id.APIVersion,
		"minimum":        MinEngineVersion,
	}

	below, ok := engineBelowFloor(id.Version, MinEngineVersion)
	switch {
	case !ok:
		// No evidence, so no verdict. A version string this cannot read
		// is a vendor spelling nobody anticipated, and refusing on one
		// would stop a plugin that works. Said out loud because a floor
		// that was never actually applied must not look like one that
		// passed.
		log.WithFields(fields).
			Warn("The Docker Engine version could not be read as a version, so the minimum was not checked")
	case below:
		return fmt.Errorf("%w: engine %s, minimum %s (measured, see docs/reference.md)",
			errEngineTooOld, id.Version, MinEngineVersion)
	default:
		log.WithFields(fields).Info("Docker Engine identified")
	}

	// The API half, which never refuses. See the block above the
	// MinEngineAPIVersion declaration.
	if apiBelow, apiOK := engineBelowFloor(id.APIVersion, MinEngineAPIVersion); apiOK && apiBelow {
		log.WithFields(log.Fields{
			"engine_version":   id.Version,
			"api_version":      id.APIVersion,
			"api_at_the_floor": MinEngineAPIVersion,
		}).Warn("The negotiated Docker API version is below the one the minimum engine reports; the engine version is what the minimum is measured on")
	}
	return nil
}

// identifyEngine asks the daemon who it is: one ping, which is also
// what makes the client negotiate, then one version query.
//
// TWO CALLS AND NOT ONE. /version alone carries the daemon's own
// APIVersion, which is its MAXIMUM and not what this client settled on;
// ClientVersion() after a ping is the negotiated value, and those are
// different numbers on any engine newer than the client library. The
// health document says `api_version`, and the version an operator
// cannot see us using is not the one to publish.
func (p *Plugin) identifyEngine(ctx context.Context) (engineIdentity, error) {
	ctx, cancel := context.WithTimeout(ctx, engineProbeTimeout)
	defer cancel()

	if _, err := p.docker.Ping(ctx); err != nil {
		return engineIdentity{}, fmt.Errorf("pinging the Docker daemon: %w", err)
	}
	v, err := p.docker.ServerVersion(ctx)
	if err != nil {
		return engineIdentity{}, fmt.Errorf("reading the Docker Engine version: %w", err)
	}
	api := p.docker.ClientVersion()
	if api == "" {
		api = unknownEngineField
	}
	if v.Version == "" {
		return engineIdentity{}, fmt.Errorf("the Docker daemon reported an empty engine version")
	}
	return engineIdentity{Version: v.Version, APIVersion: api}, nil
}

// engineSnapshot is what the health document renders. It never returns
// a zero value: a document with the fields missing and one with them
// empty are the same thing to a reader, and neither says "the probe did
// not reach the daemon".
func (p *Plugin) engineSnapshot() engineIdentity {
	if id := p.engine.Load(); id != nil {
		return *id
	}
	return engineIdentity{Version: unknownEngineField, APIVersion: unknownEngineField}
}

// MinEngineIfnameVersion is the lowest engine that applies a remote
// driver's requested container-side interface name (#125, #670).
//
// MEASURED, not read off a changelog: the same nested-daemon rig the
// engine matrix uses was pointed at each line with an endpoint carrying
// `com.docker.network.endpoint.ifname=lan0`, and the container's own
// `ip link` was read. 28.5.2 and 29.7.2 name the interface by the
// driver's prefix and index; 29.8.0 names it `lan0`. That boundary is
// moby/moby#52866, which taught libnetwork's remote proxy to pass
// DstName through instead of dropping it, and which shipped in 29.8.0.
//
// BELOW IT NOTHING FAILS, and that is the whole problem. The plugin
// returns DstName on every engine, the engine below this one ignores the
// field, and the container comes up on a working network with a name
// the operator did not ask for. Nothing in Docker reports that, so the
// plugin says it: a line at CreateEndpoint naming the engine, and the
// ifname_unsupported counter for an operator who is not reading logs.
const MinEngineIfnameVersion = "29.8"

// engineAppliesIfname reports whether the engine this process identified
// applies a requested interface name, and whether that is known at all.
//
// ok=false is a real state and not a corner: the daemon may not have
// answered at startup (#383), or may report a version string the
// comparison cannot read. Neither is evidence that the name will be
// ignored, so neither counts one.
func (p *Plugin) engineAppliesIfname() (applies, ok bool) {
	below, known := engineBelowFloor(p.engineSnapshot().Version, MinEngineIfnameVersion)
	if !known {
		return false, false
	}
	return !below, true
}

// noteIfnameRequest makes the record of a custom interface name that was
// asked for, and of whether this engine will honour it.
//
// THE STATEMENT IS THE POINT. The fallback is silent by construction:
// libnetwork drops DstName and names the link by prefix and index, the
// container works, and an operator who wrote `interface_name: lan0` in
// a compose file learns what happened by running `ip link` inside the
// container. A log line that says "Honoring custom interface name" on
// an engine that ignores it is worse than no line at all.
func (p *Plugin) noteIfnameRequest(networkID, endpointID, ifname string) {
	id := p.engineSnapshot()
	fields := log.Fields{
		"network":        shortID(networkID),
		"endpoint":       shortID(endpointID),
		"ifname":         ifname,
		"engine_version": id.Version,
	}

	applies, known := p.engineAppliesIfname()
	switch {
	case !known:
		log.WithFields(fields).Info("[CreateEndpoint] Custom interface name requested; this engine did not report a version, so whether it applies the name is unknown")
	case applies:
		log.WithFields(fields).Info("[CreateEndpoint] Honoring custom interface name")
	default:
		p.ifnameUnsupported.Add(1)
		fields["engine_applies_ifname_from"] = MinEngineIfnameVersion
		log.WithFields(fields).Warn("[CreateEndpoint] This Docker Engine ignores a remote driver's interface name, so the container interface will be named by the driver prefix instead of the requested name")
	}
}
