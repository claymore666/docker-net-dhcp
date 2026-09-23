// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import "net/http"

type apiRoute struct {
	path    string
	handler http.HandlerFunc
}

// routes is every path this driver serves. ProgramExternalConnectivity and
// RevokeExternalConnectivity stay unrouted: moby's remote driver ignores their error
// when plugins.IsNotFound, which tests the 404 status alone, so a custom NotFound
// handler would fail every container start (#646, engine 26.1.5). A 404 from
// GwAllocCheck is a real error, and the daemon calls it because GetCapabilities
// advertises gwAllocChecker, so the route and the capability ship together (#110).
func (p *Plugin) routes() []apiRoute {
	return []apiRoute{
		{"/NetworkDriver.GetCapabilities", p.apiGetCapabilities},
		{"/NetworkDriver.GwAllocCheck", p.apiGwAllocCheck},

		{"/NetworkDriver.CreateNetwork", p.apiCreateNetwork},
		{"/NetworkDriver.DeleteNetwork", p.apiDeleteNetwork},

		{"/NetworkDriver.CreateEndpoint", p.apiCreateEndpoint},
		{"/NetworkDriver.EndpointOperInfo", p.apiEndpointOperInfo},
		{"/NetworkDriver.DeleteEndpoint", p.apiDeleteEndpoint},

		{"/NetworkDriver.Join", p.apiJoin},
		{"/NetworkDriver.Leave", p.apiLeave},

		{"/IpamDriver.GetCapabilities", p.apiIpamGetCapabilities},
		{"/IpamDriver.GetDefaultAddressSpaces", p.apiIpamGetDefaultAddressSpaces},
		{"/IpamDriver.RequestPool", p.apiRequestPool},
		{"/IpamDriver.ReleasePool", p.apiReleasePool},
		{"/IpamDriver.RequestAddress", p.apiRequestAddress},
		{"/IpamDriver.ReleaseAddress", p.apiReleaseAddress},

		{"/Plugin.Health", p.apiHealth},

		// Prometheus text on the socket, unconditionally; the TCP listener is METRICS_ADDR (#651).
		{"/metrics", p.apiMetrics},
	}
}

func (p *Plugin) newServeMux() *http.ServeMux {
	mux := http.NewServeMux()
	for _, r := range p.routes() {
		mux.HandleFunc(r.path, r.handler)
	}
	return mux
}

// unroutedRPCs are the RPCs the daemon calls here that answer a bare 404, named for request capture (#644).
func unroutedRPCs() []string {
	return []string{
		"/NetworkDriver.ProgramExternalConnectivity",
		"/NetworkDriver.RevokeExternalConnectivity",
	}
}

// capturablePaths is every path request capture may name, built from the routes table (#644).
func capturablePaths(rs []apiRoute) []string {
	paths := make([]string, 0, len(rs)+2)
	for _, r := range rs {
		paths = append(paths, r.path)
	}
	return append(paths, unroutedRPCs()...)
}
