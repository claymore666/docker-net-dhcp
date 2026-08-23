// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestRoutes_UnimplementedMethodsAnswer404 pins the status code the
// daemon sees for the two libnetwork RPCs this driver does not
// implement but does receive on every container start and stop.
//
// It has to be exactly 404, and for a reason that is invisible from
// here: moby's remote driver skips both calls only when
// plugins.IsNotFound(err) says so, and that helper compares the HTTP
// status alone — it never looks at the body. A 500, or a 200 carrying
// an Err field, both come back as a hard error and every affected
// container fails to start with "driver failed programming external
// connectivity on endpoint".
//
// Nothing in the plugin chooses this today: the paths are simply
// unregistered, so http.ServeMux's default handler answers. That makes
// it exactly the kind of behaviour that a custom NotFound handler or a
// route-wrapping middleware would take away silently. This test is the
// thing that goes red.
func TestRoutes_UnimplementedMethodsAnswer404(t *testing.T) {
	mux := newTestPlugin(t).newServeMux()

	// Sanity floor: a route we DO serve must not answer 404, or the
	// assertions below would pass just as well over an empty mux.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/NetworkDriver.GetCapabilities", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GetCapabilities: got %d, want 200 — the mux under test is not the real one", rec.Code)
	}

	// Driven from unroutedRPCs() rather than a second hand-written
	// list. Request capture allowlists the same function so those calls
	// can be recorded as evidence (#644); if the two lists were
	// independent, implementing one of these RPCs would leave the other
	// list quietly describing a world that no longer exists.
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

// unroutedRPCs and routes must not contradict each other. They are two
// halves of one statement — "these we serve, those we knowingly do not"
// — and capturablePaths concatenates them, so an entry in both would
// also mean a duplicate capture name.
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

// TestRoutes_RegisteredSetIsPinned covers the opposite direction from
// the 404 test: that one stays green if a route silently disappears,
// and it turns red if one of the two unimplemented RPCs is ever
// implemented — which is a legitimate thing to do, just not a silent
// one. Pinning the whole set means either change is a deliberate edit
// here, with the comment in routes.go read on the way past.
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
