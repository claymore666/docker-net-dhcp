// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// SECURITY.md tells operators they may put a read-only proxy in front of
// the Docker socket and allow four routes, because the plugin's entire
// Docker API surface is three read-only methods. That is a UNIVERSAL
// about production code, published as a security claim, and nothing but
// this file stops it going quietly false: a `ContainerStart` added in
// pkg/plugin next year compiles, passes every other test, and turns a
// documented deployment into one that breaks -- or, worse, leaves an
// operator's proxy rules narrower than what the plugin now needs and the
// failure shows up as a lease that does not renew.
//
// So the method set is DERIVED from the interfaces and compared against
// what the document lists. Adding a method to either interface without
// touching SECURITY.md fails here, and so does editing SECURITY.md's
// table to say something the code does not do.
//
// WHAT THIS DOES NOT SEE, stated rather than discovered later. It reads
// the two narrow interface declarations and the import graph; it is not
// alias analysis. Concretely it would miss a caller that got hold of the
// concrete *client.Client some other way -- through a struct field, a
// closure, or reflection. The two clauses that make that hard are here
// as well: only ONE production file may import the Docker client
// package, and inside it the concrete client's only use is being stored
// into the narrow interface. Those are the routes a normal person
// reaches for; the rest is the honest boundary.
//
// The routes themselves are measured, not derived -- no Go source states
// that NetworkList is `GET /networks`. What this test pins is the
// mapping's DOMAIN: the set of methods that can reach the socket.

// interfaceMethods returns the method names declared by the named
// interface type in the given file.
func interfaceMethods(t *testing.T, path, name string) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok || ts.Name.Name != name {
			return true
		}
		it, ok := ts.Type.(*ast.InterfaceType)
		if !ok {
			return true
		}
		for _, m := range it.Methods.List {
			for _, id := range m.Names {
				out = append(out, id.Name)
			}
		}
		return false
	})
	if len(out) == 0 {
		t.Fatalf("interface %s in %s declares no methods -- either it was renamed or this test is "+
			"now measuring nothing, and an empty derived set would let SECURITY.md claim anything.", name, path)
	}
	sort.Strings(out)
	return out
}

// dockerAPIMethods is the union of the two narrow interfaces, minus
// Close: Close shuts the HTTP transport and sends no request, which
// SECURITY.md says in as many words.
//
// TODAY THE UNION IS REDUNDANT and that is said here rather than left
// for someone to discover: ContainerInspector declares only
// ContainerInspect, which dockerClient already declares, so removing
// pkg/util/docker.go from this function changes no verdict. It is read
// anyway because the two can diverge -- a later change that moves a
// method out of dockerClient and leaves it only on ContainerInspector
// would otherwise drop it from the derived set silently, and a derived
// set that quietly shrinks is how a published universal goes vacuous.
func dockerAPIMethods(t *testing.T) []string {
	t.Helper()
	seen := map[string]bool{}
	for _, src := range []struct{ path, name string }{
		{"docker_client.go", "dockerClient"},
		{filepath.Join("..", "util", "docker.go"), "ContainerInspector"},
	} {
		for _, m := range interfaceMethods(t, src.path, src.name) {
			if m == "Close" {
				continue
			}
			seen[m] = true
		}
	}
	out := make([]string, 0, len(seen))
	for m := range seen {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

// securityRoutes parses the fenced route table out of SECURITY.md and
// returns, per line, the verb, the path and the trailing label.
func securityRoutes(t *testing.T) [][3]string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "SECURITY.md"))
	if err != nil {
		t.Fatalf("reading SECURITY.md: %v", err)
	}
	var rows [][3]string
	inFence := false
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "```") {
			inFence = !inFence
			continue
		}
		if !inFence {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		if f[0] != "GET" && f[0] != "HEAD" {
			continue
		}
		if !strings.HasPrefix(f[1], "/") {
			continue
		}
		rows = append(rows, [3]string{f[0], f[1], strings.Join(f[2:], " ")})
	}
	return rows
}

func TestSECURITY_DocumentsExactlyTheDockerAPISurface(t *testing.T) {
	want := dockerAPIMethods(t)

	rows := securityRoutes(t)
	if len(rows) == 0 {
		t.Fatalf("SECURITY.md carries no route table this test can read. It is the published claim "+
			"about what a read-only proxy must allow; an unreadable table means the claim is "+
			"unchecked, not that it is fine. Expected fenced lines of `<VERB> <path> <label>`, "+
			"one per method in %v plus a HEAD /_ping row.", want)
	}

	var pings int
	got := map[string]bool{}
	for _, r := range rows {
		if r[2] == "API version negotiation" {
			pings++
			if r[0] != "HEAD" || r[1] != "/_ping" {
				t.Errorf("the negotiation row reads %s %s; the client sends HEAD /_ping", r[0], r[1])
			}
			continue
		}
		got[r[2]] = true
	}
	if pings != 1 {
		t.Errorf("SECURITY.md has %d rows labelled 'API version negotiation'; want exactly 1. "+
			"Without the ping the documented allow-list breaks the plugin against any daemon "+
			"older than the client's compiled default.", pings)
	}

	for _, m := range want {
		if !got[m] {
			t.Errorf("%s can reach the Docker socket but SECURITY.md does not list a route for it. "+
				"Operators build proxy allow-lists from that table; a method missing from it is a "+
				"documented deployment that fails.", m)
		}
		delete(got, m)
	}
	for m := range got {
		t.Errorf("SECURITY.md lists a route for %q, which is not a method of the narrow Docker "+
			"interfaces. Either the interface lost it -- in which case the allow-list is wider than "+
			"it needs to be -- or the table names something that never existed.", m)
	}
}

// productionFiles walks cmd/ and pkg/ and yields non-test .go files.
func productionFiles(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, root := range []string{filepath.Join("..", "..", "cmd"), filepath.Join("..", "..", "pkg")} {
		err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			out = append(out, p)
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", root, err)
		}
	}
	if len(out) == 0 {
		t.Fatal("no production Go files found -- this test would then prove nothing about the import graph")
	}
	return out
}

func TestDockerClientPackage_HasOneProductionImporter(t *testing.T) {
	fset := token.NewFileSet()
	var importers []string
	for _, p := range productionFiles(t) {
		f, err := parser.ParseFile(fset, p, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parsing %s: %v", p, err)
		}
		for _, imp := range f.Imports {
			if strings.Trim(imp.Path.Value, `"`) == "github.com/docker/docker/client" {
				importers = append(importers, filepath.ToSlash(p))
			}
		}
	}
	sort.Strings(importers)
	want := []string{"../../pkg/plugin/plugin.go"}
	if len(importers) != 1 || importers[0] != want[0] {
		t.Fatalf("production importers of the Docker client package = %v, want %v.\n"+
			"The narrow interfaces are what make SECURITY.md's read-only claim checkable, and they "+
			"only bind where the concrete client is constructed. A second importer holds a client "+
			"this test cannot see the methods of.", importers, want)
	}
}

func TestDockerClient_NegotiatesAPIVersionAndNotFromEnv(t *testing.T) {
	b, err := os.ReadFile("plugin.go")
	if err != nil {
		t.Fatalf("reading plugin.go: %v", err)
	}
	src := string(b)
	if !strings.Contains(src, "docker.WithAPIVersionNegotiation()") {
		t.Error("plugin.go no longer passes WithAPIVersionNegotiation(). SECURITY.md tells operators " +
			"their proxy MUST allow HEAD /_ping because of it; without negotiation that row is wrong " +
			"in the other direction and the allow-list is wider than it needs to be.")
	}
	if strings.Contains(src, "WithVersionFromEnv") {
		t.Error("plugin.go now passes WithVersionFromEnv(). SECURITY.md states that DOCKER_API_VERSION " +
			"is NOT an escape hatch for a proxy that blocks the ping; that sentence has become false.")
	}
}
