// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRoutes_UnimplementedMethodsAnswer404(t *testing.T) {
	mux := newTestPlugin(t).newServeMux()

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/NetworkDriver.GetCapabilities", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GetCapabilities: got %d, want 200 — the mux under test is not the real one", rec.Code)
	}

	unimplemented := append(unroutedRPCs(), "/NetworkDriver.NoSuchMethodExists")

	for _, path := range unimplemented {
		t.Run(path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, nil))
			if rec.Code != http.StatusNotFound {
				t.Errorf("%s: got %d, want 404 — libnetwork tolerates an unimplemented "+
					"RPC on the status code alone, so any other answer fails the "+
					"container operation that triggered it", path, rec.Code)
			}
		})
	}
}

func TestRoutes_UnroutedRPCsAreNotAlsoRouted(t *testing.T) {
	served := map[string]bool{}
	for _, r := range (&Plugin{}).routes() {
		served[r.path] = true
	}
	for _, path := range unroutedRPCs() {
		if served[path] {
			t.Errorf("%s is listed as deliberately unimplemented but is also served. "+
				"If it was implemented on purpose, drop it from unroutedRPCs() and "+
				"update the comment in routes.go that says why it is absent.", path)
		}
	}
	if len(unroutedRPCs()) == 0 {
		t.Error("unroutedRPCs() is empty; the 404 contract test would then assert " +
			"nothing but the synthetic path")
	}
}

func TestRoutes_RegisteredSetIsPinned(t *testing.T) {
	want := []string{
		"/NetworkDriver.GetCapabilities",
		"/NetworkDriver.CreateNetwork",
		"/NetworkDriver.DeleteNetwork",
		"/NetworkDriver.CreateEndpoint",
		"/NetworkDriver.EndpointOperInfo",
		"/NetworkDriver.DeleteEndpoint",
		"/NetworkDriver.Join",
		"/NetworkDriver.Leave",
		"/NetworkDriver.GwAllocCheck",
		"/IpamDriver.GetCapabilities",
		"/IpamDriver.GetDefaultAddressSpaces",
		"/IpamDriver.RequestPool",
		"/IpamDriver.ReleasePool",
		"/IpamDriver.RequestAddress",
		"/IpamDriver.ReleaseAddress",
		"/Plugin.Health",
		"/metrics",
	}

	got := map[string]bool{}
	for _, r := range newTestPlugin(t).routes() {
		if r.handler == nil {
			t.Errorf("%s: registered with a nil handler", r.path)
		}
		if got[r.path] {
			t.Errorf("%s: registered twice", r.path)
		}
		got[r.path] = true
	}

	for _, p := range want {
		if !got[p] {
			t.Errorf("%s: no longer served", p)
		}
		delete(got, p)
	}
	for p := range got {
		t.Errorf("%s: newly served — if this is one of the RPCs routes.go "+
			"documents as deliberately unimplemented, that comment and "+
			"TestRoutes_UnimplementedMethodsAnswer404 need updating too", p)
	}
}
