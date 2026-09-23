// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// A refusal names no release version, since it holds in every release that carries it (#817).

var releaseVersionPattern = regexp.MustCompile(`\bv[0-9]+\.[0-9]+(\.[0-9]+)?\b`)

func releaseVersionIn(s string) string {
	return releaseVersionPattern.FindString(s)
}

type errorConstruction struct {
	file string
	line int
	text string
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("resolving the package directory: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod above %s, so this rule has no module to quantify over", dir)
		}
		dir = parent
	}
}

func moduleSourceFiles(t *testing.T) (root string, files []string) {
	t.Helper()
	root = moduleRoot(t)
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if strings.HasPrefix(d.Name(), ".") || d.Name() == "vendor" || d.Name() == "test" {
				return fs.SkipDir
			}
			return nil
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return rerr
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	sort.Strings(files)
	return root, files
}

func errorConstructionsIn(t *testing.T, root, rel string) (out []errorConstruction, versionComments int) {
	t.Helper()
	src, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatalf("reading %s: %v", rel, err)
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, rel, src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parsing %s: %v", rel, err)
	}
	for _, group := range f.Comments {
		for _, c := range group.List {
			if releaseVersionIn(c.Text) != "" {
				versionComments++
			}
		}
	}
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		switch pkg.Name + "." + sel.Sel.Name {
		case "fmt.Errorf", "errors.New":
		default:
			return true
		}
		start := fset.Position(call.Pos())
		out = append(out, errorConstruction{
			file: rel,
			line: start.Line,
			text: string(src[start.Offset:fset.Position(call.End()).Offset]),
		})
		return true
	})
	return out, versionComments
}

func countDirs(files []string) int {
	seen := map[string]bool{}
	for _, f := range files {
		seen[filepath.Dir(f)] = true
	}
	return len(seen)
}

func errorConstructionsNaming(t *testing.T, phrase string) int {
	t.Helper()
	root, files := moduleSourceFiles(t)
	n := 0
	for _, rel := range files {
		got, _ := errorConstructionsIn(t, root, rel)
		for _, e := range got {
			if strings.Contains(e.text, phrase) {
				n++
			}
		}
	}
	return n
}

func TestRefusals_NameNoReleaseVersion(t *testing.T) {
	root, files := moduleSourceFiles(t)
	if len(files) == 0 {
		t.Fatal("no non-test Go files were found in this module, so this rule quantifies " +
			"over nothing and would pass on an empty tree")
	}

	var all []errorConstruction
	comments := 0
	for _, rel := range files {
		got, c := errorConstructionsIn(t, root, rel)
		all = append(all, got...)
		comments += c
	}
	if len(all) == 0 {
		t.Fatal("no fmt.Errorf or errors.New call was found in the whole module, which cannot " +
			"be true. The extractor is broken and this rule is judging nothing")
	}

	named := 0
	for _, e := range all {
		v := releaseVersionIn(e.text)
		if v == "" {
			continue
		}
		named++
		t.Errorf("%s:%d builds an error naming the release %s:\n\t%s\n\n"+
			"A refusal holds in every release that carries it, so the version "+
			"tells the operator nothing and is wrong at the next tag. State the "+
			"condition, not the release it was written in.",
			e.file, e.line, v, strings.Join(strings.Fields(e.text), " "))
	}

	t.Logf("PASS  no release version in %d error construction(s) over %d module file(s) in "+
		"%d director(ies); %d comment line(s) name a version and are not judged",
		len(all), len(files), countDirs(files), comments)

	if named != 0 {
		t.Errorf("%d error construction(s) named a release version", named)
	}
}

func TestRefusalVersionMatchers_SeeTheRealShapes(t *testing.T) {
	for _, s := range []string{
		"`-o ipv6=true` cannot be combined with this plugin as the IPAM driver in v2.1.0.",
		"ipvlan networks cannot use this plugin as an IPAM driver in v2.1.0, because",
		"gone in v2.0.0",
		"since v1.5.0",
		"v2.2.0",
	} {
		if releaseVersionIn(s) == "" {
			t.Errorf("releaseVersionIn(%q) found nothing, so a refusal written that way is "+
				"invisible to the rule", s)
		}
	}
	for _, s := range []string{
		"RFC 4861 section 4.2",
		"Progress on IPv6 in IPAM mode is tracked in issue #960",
		"a DHCPv6 address",
		"the v6 client",
		"192.168.99.0/24",
		"",
	} {
		if v := releaseVersionIn(s); v != "" {
			t.Errorf("releaseVersionIn(%q) reported %q, which is not a release version", s, v)
		}
	}

	dir := t.TempDir()
	const sample = `package sample

import (
	"errors"
	"fmt"
)

// A comment naming v9.9.9 is not judged.
func a() error {
	return fmt.Errorf("%w: ipv6=false and ipv6_mode=slaac contradict each other: "+
		"ipv6_mode switches IPv6 on for every endpoint, since v2.3.0. "+
		"Set ipv6_mode=off", errors.New("x"))
}

func b() error { return errors.New("plain, no version") }

func c() error { return fmt.Errorf("issue #960 and RFC 4861") }
`
	if err := os.WriteFile(filepath.Join(dir, "sample.go"), []byte(sample), 0o600); err != nil {
		t.Fatalf("writing the sample: %v", err)
	}
	got, comments := errorConstructionsIn(t, dir, "sample.go")
	if len(got) != 4 {
		t.Fatalf("the extractor found %d error construction(s) in the sample, want 4 "+
			"(two in a(), one in b(), one in c()): %v", len(got), got)
	}
	if comments != 1 {
		t.Errorf("the extractor counted %d version-naming comment(s) in the sample, want 1. "+
			"The receipt's exclusion number is what makes the comment bound visible", comments)
	}
	hits := map[int]string{}
	for _, e := range got {
		if v := releaseVersionIn(e.text); v != "" {
			hits[e.line] = v
		}
	}
	if len(hits) != 1 || hits[10] != "v2.3.0" {
		t.Errorf("the rule saw %v on the sample. It must see v2.3.0 on the continuation line "+
			"of the call that starts at line 10, and nothing else: a comment is not "+
			"judged, and `#960` and `RFC 4861` are not versions", hits)
	}

	_, files := moduleSourceFiles(t)
	want := map[string]bool{
		"pkg/plugin/ipam_mode.go": false,
		"pkg/plugin/ipv6_mode.go": false,
		"pkg/dhcp/v6mode.go":      false,
		"cmd/net-dhcp/main.go":    false,
	}
	for _, f := range files {
		if _, ok := want[f]; ok {
			want[f] = true
		}
		if strings.HasSuffix(f, "_test.go") {
			t.Errorf("moduleSourceFiles returned the test file %s, so the rule would judge "+
				"the very examples this test writes down", f)
		}
		if strings.HasPrefix(f, "vendor/") || strings.HasPrefix(f, "test/") {
			t.Errorf("moduleSourceFiles returned %s, which the header states is outside the "+
				"rule", f)
		}
	}
	for f, found := range want {
		if !found {
			t.Errorf("moduleSourceFiles did not return %s, which carries a refusal an "+
				"operator reaches. The domain is not what it claims to be; it has %d "+
				"file(s)", f, len(files))
		}
	}
}
