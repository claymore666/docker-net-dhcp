// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"fmt"
	"time"

	log "github.com/sirupsen/logrus"
)

// engineProbeTimeout matches the client's per-call timeout in newDockerClient (#670).
const engineProbeTimeout = 2 * time.Second

// unknownEngineField is a word, never empty: an empty value reads as nothing to report (#670).
const unknownEngineField = "unknown"

// The floor compares the engine version, not the negotiated API version: the negotiated value is
// min(client maximum, daemon maximum), and a socket proxy pinning an old API says nothing about the
// engine behind it. An API below the floor row's only warns (#670).

// MinEngineAPIVersion is the API the floor row reported (engine-matrix run 34594749584), and nothing refuses on it
// (#670).
const MinEngineAPIVersion = "1.41"

// probeEngine refuses a daemon below the floor; a daemon that does not answer is not refused, because
// Docker starts the plugin before it serves (#383), and reprobeEngine checks again later (#670).
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

// reprobeEngine logs a below-floor engine and does not refuse: the process may already be serving (#670).
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

func (p *Plugin) judgeEngine(id engineIdentity) error {
	fields := log.Fields{
		"engine_version": id.Version,
		"api_version":    id.APIVersion,
		"minimum":        MinEngineVersion,
	}

	below, ok := engineBelowFloor(id.Version, MinEngineVersion)
	switch {
	case !ok:
		// An unreadable version string gives no verdict, and says so (#670).
		log.WithFields(fields).
			Warn("The Docker Engine version could not be read as a version, so the minimum was not checked")
	case below:
		return fmt.Errorf("%w: engine %s, minimum %s (measured, see docs/reference.md)",
			errEngineTooOld, id.Version, MinEngineVersion)
	default:
		log.WithFields(fields).Info("Docker Engine identified")
	}

	if apiBelow, apiOK := engineBelowFloor(id.APIVersion, MinEngineAPIVersion); apiOK && apiBelow {
		log.WithFields(log.Fields{
			"engine_version":   id.Version,
			"api_version":      id.APIVersion,
			"api_at_the_floor": MinEngineAPIVersion,
		}).Warn("The negotiated Docker API version is below the one the minimum engine reports; the engine version is what the minimum is measured on")
	}
	return nil
}

// /version reports the daemon's maximum API; ClientVersion after a ping is the negotiated one the health
// document publishes (#670).
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

func (p *Plugin) engineSnapshot() engineIdentity {
	if id := p.engine.Load(); id != nil {
		return *id
	}
	return engineIdentity{Version: unknownEngineField, APIVersion: unknownEngineField}
}

// MinEngineIfnameVersion is the lowest engine measured to apply a requested interface name: 28.5.2 and 29.7.2 ignored
// it and 29.8.0 applied it (moby/moby#52866, #125, #670).
const MinEngineIfnameVersion = "29.8"

// engineVersionAppliesIfname returns ok=false when the daemon did not answer (#383) or its version is unreadable
// (#670).
func (p *Plugin) engineVersionAppliesIfname() (applies, ok bool) {
	below, known := engineBelowFloor(p.engineSnapshot().Version, MinEngineIfnameVersion)
	if !known {
		return false, false
	}
	return !below, true
}

// noteIfnameRequest logs and counts a requested name this engine will ignore: libnetwork drops DstName silently (#125).
func (p *Plugin) noteIfnameRequest(networkID, endpointID, ifname string) {
	id := p.engineSnapshot()
	fields := log.Fields{
		"network":        shortID(networkID),
		"endpoint":       shortID(endpointID),
		"ifname":         ifname,
		"engine_version": id.Version,
	}

	applies, known := p.engineVersionAppliesIfname()
	switch {
	case !known:
		log.WithFields(fields).Info("[CreateEndpoint] Custom interface name requested; this engine did not report a version, so whether it applies the name is unknown")
	case applies:
		log.WithFields(fields).Info("[CreateEndpoint] Honoring custom interface name")
	default:
		p.ifnameUnsupported.Add(1)
		fields["engine_applies_ifname_from"] = MinEngineIfnameVersion
		log.WithFields(fields).Warn("[CreateEndpoint] This Docker Engine is older than the first that applies a remote driver's interface name, so the container interface will be named by the driver prefix and index instead of the requested name")
	}
}
