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

	unimplemented := []struct {
		path string
		why  string
	}{
		{
			"/NetworkDriver.ProgramExternalConnectivity",
			"sent on every container start; tolerated by libnetwork only on 404",
		},
		{
			"/NetworkDriver.RevokeExternalConnectivity",
			"sent on every container stop; tolerated by libnetwork only on 404",
		},
		{
			"/NetworkDriver.NoSuchMethodExists",
			"an RPC from a future engine we have never seen",
		},
	}
	for _, c := range unimplemented {
		t.Run(c.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, c.path, nil))
			if rec.Code != http.StatusNotFound {
				t.Errorf("%s: got %d, want 404 (%s)", c.path, rec.Code, c.why)
			}
		})
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
