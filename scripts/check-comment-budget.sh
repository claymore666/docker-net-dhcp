#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Comment budget on Go files (#1056). On the lines a range adds, a comment
# block is at most 10 lines and a block of 2 or more names #N or RFC N;
# per package, comment/(comment+code) lines may not rise over the merge base.
# --prove requires every changed Go file to keep its code tokens (#1056).
#
# Usage: check-comment-budget.sh [<base>..<head>]
#        check-comment-budget.sh --prove <base> <head>
# Exit:  0 clean or no range to judge, 1 fail, 2 cannot check.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=scripts/tmpdir-guard.sh
. "$HERE/tmpdir-guard.sh"

die2() { echo "FAIL  cannot check: $*" >&2; exit 2; }

commit_of() {
    git rev-parse -q --verify "$1^{commit}" 2>/dev/null || die2 "cannot resolve '$1'"
}

extract() { # REV DIR
    mkdir -p "$2" || die2 "cannot create $2"
    git archive --format=tar "$1" | tar -x -C "$2" || die2 "cannot extract $1"
}

build_helper() { # DIR
    command -v go >/dev/null 2>&1 || die2 "go is not on PATH"
    mkdir -p "$1/src" || die2 "cannot create $1/src"
    printf 'module commentbudget\n\ngo 1.22\n' > "$1/src/go.mod"
    cat > "$1/src/main.go" <<'GO'
package main

import (
	"bufio"
	"bytes"
	"fmt"
	"go/parser"
	"go/scanner"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const (
	blank   = 'B'
	code    = 'K'
	comment = 'C'
	neutral = 'N'
	header  = 'H'
	pkgdoc  = 'P'
)

var refRE = regexp.MustCompile(`#[0-9]+|RFC [0-9]+`)

type file struct {
	class []byte
	text  []string
}

func fail2(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "FAIL  cannot check: "+format+"\n", a...)
	os.Exit(2)
}

func isDirective(c string) bool {
	for _, p := range []string{"//go:", "//nolint", "//line ", "// +build"} {
		if strings.HasPrefix(c, p) {
			return true
		}
	}
	return false
}

func commentLineText(l string) string {
	t := strings.TrimSpace(l)
	t = strings.TrimPrefix(t, "//")
	t = strings.TrimPrefix(t, "/*")
	t = strings.TrimSuffix(t, "*/")
	return strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(t), "*"))
}

func classify(name string, src []byte) file {
	lines := strings.Split(string(src), "\n")
	n := len(lines)
	hasCode := make([]bool, n+2)
	hasText := make([]bool, n+2)
	hasDir := make([]bool, n+2)
	inCmt := make([]bool, n+2)
	fset := token.NewFileSet()
	tf := fset.AddFile(name, -1, len(src))
	var s scanner.Scanner
	errs := 0
	s.Init(tf, src, func(token.Position, string) { errs++ }, scanner.ScanComments)
	for {
		pos, tok, lit := s.Scan()
		if tok == token.EOF {
			break
		}
		if tok == token.SEMICOLON && lit == "\n" {
			continue
		}
		start := fset.PositionFor(pos, false).Line
		switch tok {
		case token.COMMENT:
			if isDirective(lit) {
				hasDir[start] = true
				continue
			}
			for i, l := range strings.Split(lit, "\n") {
				inCmt[start+i] = true
				if commentLineText(l) != "" {
					hasText[start+i] = true
				}
			}
		case token.STRING, token.CHAR:
			for i := 0; i <= strings.Count(lit, "\n"); i++ {
				hasCode[start+i] = true
			}
		default:
			hasCode[start] = true
		}
	}
	if errs > 0 {
		fail2("%s does not scan as Go", name)
	}
	f := file{class: make([]byte, n+1), text: append([]string{""}, lines...)}
	for i := 1; i <= n; i++ {
		switch {
		case hasCode[i]:
			f.class[i] = code
		case hasText[i]:
			f.class[i] = comment
		case strings.TrimSpace(lines[i-1]) != "" || hasDir[i] || inCmt[i]:
			f.class[i] = neutral
		default:
			f.class[i] = blank
		}
	}
	markHeader(&f)
	if filepath.Base(name) == "doc.go" {
		markPkgDoc(&f, name, src)
	}
	return f
}

// markHeader exempts the licence block ahead of the package clause (#1056).
func markHeader(f *file) {
	for i := 1; i < len(f.class) && f.class[i] != code; {
		if f.class[i] == blank {
			i++
			continue
		}
		start, spdx := i, false
		for ; i < len(f.class) && f.class[i] != code && f.class[i] != blank; i++ {
			spdx = spdx || strings.Contains(f.text[i], "SPDX-License-Identifier")
		}
		for j := start; spdx && j < i; j++ {
			f.class[j] = header
		}
	}
}

func markPkgDoc(f *file, name string, src []byte) {
	fset := token.NewFileSet()
	af, err := parser.ParseFile(fset, name, src, parser.ParseComments|parser.PackageClauseOnly)
	if err != nil {
		fail2("%s does not parse: %v", name, err)
	}
	if af.Doc == nil {
		return
	}
	for i := fset.PositionFor(af.Doc.Pos(), false).Line; i <= fset.PositionFor(af.Doc.End(), false).Line; i++ {
		if f.class[i] == comment {
			f.class[i] = pkgdoc
		}
	}
}

type share struct{ c, k int }

func walk(root string) map[string]share {
	out := map[string]share{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "testdata" || d.Name() == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") {
			return nil
		}
		src, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		f := classify(rel, src)
		s := out[filepath.Dir(rel)]
		for _, c := range f.class {
			switch c {
			case comment:
				s.c++
			case code:
				s.k++
			}
		}
		out[filepath.Dir(rel)] = s
		return nil
	})
	if err != nil {
		fail2("%v", err)
	}
	return out
}

func pct(s share) string {
	if s.c+s.k == 0 {
		return "-"
	}
	return fmt.Sprintf("%.1f%% (%d/%d)", 100*float64(s.c)/float64(s.c+s.k), s.c, s.c+s.k)
}

// check reads "path:line" added lines from stdin and judges head against base.
func check(base, head string) int {
	added := map[string]map[int]bool{}
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		i := strings.LastIndexByte(sc.Text(), ':')
		if i < 0 {
			continue
		}
		n, err := strconv.Atoi(sc.Text()[i+1:])
		if err != nil {
			fail2("bad added-line record %q", sc.Text())
		}
		p := sc.Text()[:i]
		if added[p] == nil {
			added[p] = map[int]bool{}
		}
		added[p][n] = true
	}
	bad := 0
	commented := map[string]bool{}
	paths := make([]string, 0, len(added))
	for p := range added {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		src, err := os.ReadFile(filepath.Join(head, p))
		if err != nil {
			fail2("%v", err)
		}
		f := classify(p, src)
		for i := range f.class {
			if f.class[i] == comment && added[p][i] {
				commented[filepath.Dir(p)] = true
			}
		}
		for i := 1; i < len(f.class); {
			if f.class[i] != comment && f.class[i] != pkgdoc {
				i++
				continue
			}
			start, count, touched, ref, doc := i, 0, false, false, false
			for ; i < len(f.class) && (f.class[i] == comment || f.class[i] == pkgdoc || f.class[i] == neutral); i++ {
				if f.class[i] == neutral {
					continue
				}
				count++
				touched = touched || added[p][i]
				ref = ref || refRE.MatchString(f.text[i])
				doc = doc || f.class[i] == pkgdoc
			}
			if !touched {
				continue
			}
			if count > 10 && !doc {
				fmt.Printf("FAIL  %s:%d: comment block of %d lines; the limit is 10\n", p, start, count)
				bad++
			}
			if count >= 2 && !ref {
				fmt.Printf("FAIL  %s:%d: comment block of %d lines carries no #N or RFC N reference\n", p, start, count)
				bad++
			}
		}
	}
	b, h := walk(base), walk(head)
	pkgs := map[string]bool{}
	for p := range b {
		pkgs[p] = true
	}
	for p := range h {
		pkgs[p] = true
	}
	names := make([]string, 0, len(pkgs))
	for p := range pkgs {
		names = append(names, p)
	}
	sort.Strings(names)
	fmt.Println("comment share per package, comment lines / (comment + code lines):")
	fmt.Printf("  %-40s %-22s %-22s %s\n", "package", "base", "head", "verdict")
	for _, p := range names {
		bs, bok := b[p]
		hs, hok := h[p]
		v := "ok"
		switch {
		case !hok:
			v = "removed"
		case !bok:
			v = "new"
		case hs.c*(bs.c+bs.k) <= bs.c*(hs.c+hs.k):
		case !commented[p]:
			// A rise with no added comment line, as from deleting code, passes (#1057).
			v = "rose, no comment added"
		default:
			v = "FAIL rose"
			bad++
		}
		fmt.Printf("  %-40s %-22s %-22s %s\n", p, pct(bs), pct(hs), v)
	}
	if bad > 0 {
		return 1
	}
	return 0
}

// canon is the file's token stream with comments dropped; directives stay, being code to the toolchain.
func canon(name string, src []byte) []byte {
	fset := token.NewFileSet()
	af, err := parser.ParseFile(fset, name, src, parser.ImportsOnly)
	if err != nil {
		fail2("%s does not parse: %v", name, err)
	}
	for _, im := range af.Imports {
		if im.Path.Value == `"C"` {
			fail2("%s imports C; its preamble comment is code", name)
		}
	}
	if _, err := parser.ParseFile(token.NewFileSet(), name, src, 0); err != nil {
		fail2("%s does not parse: %v", name, err)
	}
	var out bytes.Buffer
	var s scanner.Scanner
	s.Init(fset.AddFile(name, -1, len(src)), src, nil, scanner.ScanComments)
	for {
		_, tok, lit := s.Scan()
		if tok == token.EOF {
			break
		}
		if tok == token.COMMENT && !isDirective(lit) {
			continue
		}
		fmt.Fprintf(&out, "%s %q\n", tok, lit)
	}
	return out.Bytes()
}

func prove(base, head string, names []string) int {
	bad := 0
	for _, n := range names {
		bs, berr := os.ReadFile(filepath.Join(base, n))
		hs, herr := os.ReadFile(filepath.Join(head, n))
		switch {
		case berr != nil || herr != nil:
			fmt.Printf("DIFFERS  %s: present on one side only\n", n)
			bad++
		case !bytes.Equal(canon(n, bs), canon(n, hs)):
			fmt.Printf("DIFFERS  %s: code tokens changed\n", n)
			bad++
		default:
			fmt.Printf("same     %s\n", n)
		}
	}
	if bad > 0 {
		return 1
	}
	return 0
}

func main() {
	if len(os.Args) < 4 {
		fail2("usage: helper check|prove <base-dir> <head-dir> [file...]")
	}
	switch os.Args[1] {
	case "check":
		os.Exit(check(os.Args[2], os.Args[3]))
	case "prove":
		os.Exit(prove(os.Args[2], os.Args[3], os.Args[4:]))
	}
	fail2("unknown mode %q", os.Args[1])
}
GO
    (cd "$1/src" && GOTOOLCHAIN=local GOWORK=off GOFLAGS='' go build -o "$1/cb" .) \
        || die2 "the helper did not build"
}

guarded_tmpdir WORK
build_helper "$WORK"

if [ "${1-}" = "--prove" ]; then
    [ "$#" -eq 3 ] || die2 "usage: $0 --prove <base> <head>"
    base=$(commit_of "$2") || exit 2
    head=$(commit_of "$3") || exit 2
    mapfile -t files < <(git -c core.quotePath=false diff --name-only --no-renames "$base" "$head" -- '*.go' \
        | grep -Ev '(^|/)(testdata|vendor)/')
    if [ "${#files[@]}" -eq 0 ]; then
        echo "comment-budget proof: no Go file changed between $2 and $3"
        exit 0
    fi
    extract "$base" "$WORK/base"
    extract "$head" "$WORK/head"
    "$WORK/cb" prove "$WORK/base" "$WORK/head" "${files[@]}"
    rc=$?
    [ "$rc" -eq 0 ] && echo "comment-budget proof: code tokens identical in ${#files[@]} file(s)"
    exit "$rc"
fi

[ "$#" -le 1 ] || die2 "usage: $0 [<base>..<head>]"
RANGE="${1-}"
# A push, tag or scheduled build has no pull-request range; same inputs as the weakening gate (#413, #1056).
if [ -z "$RANGE" ]; then
    echo "comment-budget: SKIP, no commit range at this stage (not a pull request)"
    exit 0
fi
case "$RANGE" in
    *...*) a="${RANGE%%...*}"; b="${RANGE#*...}" ;;
    *..*)  a="${RANGE%%..*}";  b="${RANGE#*..}" ;;
    *)     die2 "'$RANGE' is not a <base>..<head> range" ;;
esac
a=$(commit_of "${a:-HEAD}") || exit 2
b=$(commit_of "${b:-HEAD}") || exit 2
if ! mb=$(git merge-base "$a" "$b" 2>/dev/null); then
    [ "$(git rev-parse --is-shallow-repository)" = "true" ] \
        && die2 "no merge base in a shallow clone; fetch the history first"
    echo "comment-budget: SKIP, '$RANGE' has no merge base"
    exit 0
fi

echo "comment-budget: judging $(git rev-parse --short "$mb")..$(git rev-parse --short "$b")"
extract "$mb" "$WORK/base"
extract "$b" "$WORK/head"
git -c core.quotePath=false diff -U0 -M --no-color --diff-filter=d "$mb" "$b" -- '*.go' > "$WORK/diff" \
    || die2 "cannot diff $mb $b"
LC_ALL=C awk '
    /^diff --git / { hdr = 1; next }
    hdr && /^\+\+\+ / { path = substr($0, 7); next }
    /^@@ / {
        hdr = 0
        split($3, h, ","); start = substr(h[1], 2) + 0; n = (h[2] == "" ? 1 : h[2] + 0)
        for (i = 0; i < n; i++) print path ":" (start + i)
    }' "$WORK/diff" | grep -Ev '(^|/)(testdata|vendor)/' > "$WORK/added"
"$WORK/cb" check "$WORK/base" "$WORK/head" < "$WORK/added"
rc=$?
[ "$rc" -eq 0 ] && echo "comment-budget gate passed"
exit "$rc"
