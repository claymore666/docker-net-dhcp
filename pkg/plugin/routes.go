// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import "net/http"

// apiRoute is one entry in the plugin's HTTP routing table.
type apiRoute struct {
	path    string
	handler http.HandlerFunc
}

// routes is the complete set of paths this driver answers on its
// socket — the single source of truth, so that what we serve and what
// we deliberately do not serve are both readable in one place.
//
// Two libnetwork RPCs are deliberately absent even though the daemon
// calls them on every container start and stop (#646, captured on
// engine 26.1.5):
//
//	/NetworkDriver.ProgramExternalConnectivity
//	/NetworkDriver.RevokeExternalConnectivity
//
// Nothing routes them, so http.ServeMux answers its default bare 404,
// and libnetwork's remote driver reads that as "driver does not
// implement this" and carries on — moby's
// libnetwork/drivers/remote/driver.go guards both calls with
//
//	if err != nil && plugins.IsNotFound(err) { return nil }
//
// and plugins.IsNotFound tests the status code alone, never the body.
// So the 404 status is the contract, not an accident of routing: give
// this mux a custom NotFound handler that answers anything else and
// every container start fails with "driver failed programming external
// connectivity on endpoint". Nothing else in this repo would go red
// for that — TestRoutes_UnimplementedMethodsAnswer404 exists for
// exactly this.
//
// The other RPCs the remote driver can emit — AllocateNetwork,
// FreeNetwork, DiscoverNew, DiscoverDelete, GwAllocCheck — carry no
// such tolerance; a 404 from those propagates as a real error. They
// are unreachable for us rather than tolerated: the first four are
// swarm / node-discovery paths, and GwAllocCheck is only called when
// GetCapabilities advertises gwAllocChecker, which ours does not.
func (p *Plugin) routes() []apiRoute {
	return []apiRoute{
		{"/NetworkDriver.GetCapabilities", p.apiGetCapabilities},

		{"/NetworkDriver.CreateNetwork", p.apiCreateNetwork},
		{"/NetworkDriver.DeleteNetwork", p.apiDeleteNetwork},

		{"/NetworkDriver.CreateEndpoint", p.apiCreateEndpoint},
		{"/NetworkDriver.EndpointOperInfo", p.apiEndpointOperInfo},
		{"/NetworkDriver.DeleteEndpoint", p.apiDeleteEndpoint},

		{"/NetworkDriver.Join", p.apiJoin},
		{"/NetworkDriver.Leave", p.apiLeave},

		// Plugin observability — not part of the libnetwork RPC
		// contract, but lives on the same socket so anything that can
		// talk to the plugin can also poll its state.
		{"/Plugin.Health", p.apiHealth},
	}
}

// newServeMux builds the plugin's request router from routes().
func (p *Plugin) newServeMux() *http.ServeMux {
	mux := http.NewServeMux()
	for _, r := range p.routes() {
		mux.HandleFunc(r.path, r.handler)
	}
	return mux
}

// unroutedRPCs are libnetwork RPCs the daemon DOES call on this socket
// and we deliberately do not serve — see the routes() comment for why
// the resulting bare 404 is the contract rather than an accident.
//
// They are listed rather than described because two things need them by
// name: TestRoutes_UnimplementedMethodsAnswer404, which pins the 404,
// and request capture (#644), which must record them under their real
// names. A capture that filed them as "unknown" would erase precisely
// the evidence that established this contract in the first place.
func unroutedRPCs() []string {
	return []string{
		"/NetworkDriver.ProgramExternalConnectivity",
		"/NetworkDriver.RevokeExternalConnectivity",
	}
}

// capturablePaths is every path request capture may name: the ones we
// serve, plus the ones we knowingly do not (#644).
//
// It is built from the SAME table the mux is built from, so a route
// added to routes() is captured without anyone remembering a second
// place. Anything outside both lists is captured as "unknown" — the
// filename can only ever be a constant from this file, never a string
// from the request.
func capturablePaths(rs []apiRoute) []string {
	paths := make([]string, 0, len(rs)+2)
	for _, r := range rs {
		paths = append(paths, r.path)
	}
	return append(paths, unroutedRPCs()...)
}
