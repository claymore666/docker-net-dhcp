#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Comment budget on Go files (#1056). On the lines a range adds, a block is
# at most 10 lines and a block of 2 or more names #N or RFC N; a package's
# comment share may not rise over the merge base. --prove, and --prove-marked
# when PR_BODY has a "Comments-only: yes" line, require unchanged code tokens.
# --whole applies the block rules to every line of the named Go files.
#
# Usage: check-comment-budget.sh [<base>..<head>]
#        check-comment-budget.sh --prove <base> <head>
#        check-comment-budget.sh --prove-marked [<base> <head>]
#        check-comment-budget.sh --whole <rev> [<path>...]
# Exit:  0 clean or no range to judge, 1 fail, 2 cannot check.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=scripts/tmpdir-guard.sh
. "$HERE/tmpdir-guard.sh"

die2() { echo "FAIL  cannot check: $*" >&2; exit 2; }

# git archive from a subdirectory would extract only that subtree.
if top=$(git rev-parse --show-toplevel 2>/dev/null); then cd "$top" || die2 "cannot enter $top"; fi

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
	trail   = 'T'
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
		case hasCode[i] && hasText[i]:
			f.class[i] = trail
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
	for i := 1; i < len(f.class) && f.class[i] != code && f.class[i] != trail; {
		if f.class[i] == blank {
			i++
			continue
		}
		start, spdx := i, false
		for ; i < len(f.class) && f.class[i] != code && f.class[i] != trail && f.class[i] != blank; i++ {
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
			case comment, trail:
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

// blocks judges the comment blocks of root/p that hold a touched line; a trailing comment is a one-line block and cannot fail.
func blocks(root, p string, touched func(int) bool) (bad int, commented bool) {
	src, err := os.ReadFile(filepath.Join(root, p))
	if err != nil {
		fail2("%v", err)
	}
	f := classify(p, src)
	for i := range f.class {
		if (f.class[i] == comment || f.class[i] == trail) && touched(i) {
			commented = true
		}
	}
	for i := 1; i < len(f.class); {
		if f.class[i] != comment && f.class[i] != pkgdoc {
			i++
			continue
		}
		start, count, hit, ref, doc := i, 0, false, false, false
		for ; i < len(f.class) && (f.class[i] == comment || f.class[i] == pkgdoc || f.class[i] == neutral); i++ {
			if f.class[i] == neutral {
				continue
			}
			count++
			hit = hit || touched(i)
			ref = ref || refRE.MatchString(f.text[i])
			doc = doc || f.class[i] == pkgdoc
		}
		if !hit {
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
	return bad, commented
}

// whole judges every line of the named files and prints their packages' shares.
func whole(root string, names []string) int {
	bad := 0
	pkgs := map[string]bool{}
	for _, p := range names {
		b, _ := blocks(root, p, func(int) bool { return true })
		bad += b
		pkgs[filepath.Dir(p)] = true
	}
	shares := walk(root)
	dirs := make([]string, 0, len(pkgs))
	for p := range pkgs {
		dirs = append(dirs, p)
	}
	sort.Strings(dirs)
	fmt.Println("comment share per package, comment lines / (comment + code lines):")
	for _, p := range dirs {
		fmt.Printf("  %-40s %s\n", p, pct(shares[p]))
	}
	fmt.Printf("comment-budget whole: %d file(s), %d failure(s)\n", len(names), bad)
	if bad > 0 {
		return 1
	}
	return 0
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
		b, c := blocks(head, p, func(i int) bool { return added[p][i] })
		bad += b
		commented[filepath.Dir(p)] = commented[filepath.Dir(p)] || c
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
		fail2("usage: helper check|prove <base-dir> <head-dir> [file...] | whole <dir> <file>...")
	}
	switch os.Args[1] {
	case "check":
		os.Exit(check(os.Args[2], os.Args[3]))
	case "prove":
		os.Exit(prove(os.Args[2], os.Args[3], os.Args[4:]))
	case "whole":
		os.Exit(whole(os.Args[2], os.Args[3:]))
	}
	fail2("unknown mode %q", os.Args[1])
}
GO
    (cd "$1/src" && GOTOOLCHAIN=local GOWORK=off GOFLAGS='' go build -o "$1/cb" .) \
        || die2 "the helper did not build"
}

guarded_tmpdir WORK
build_helper "$WORK"

# prove_files BASE HEAD LABEL [EXEMPT...]: the token proof over the Go files BASE..HEAD changes.
prove_files() {
    local base="$1" head="$2" label="$3" f e keep
    shift 3
    mapfile -t all < <(git -c core.quotePath=false diff --name-only --no-renames "$base" "$head" -- '*.go' \
        | grep -Ev '(^|/)(testdata|vendor)/')
    files=()
    for f in "${all[@]}"; do
        keep=1
        for e in "$@"; do [ "$f" = "$e" ] && keep=0; done
        [ "$keep" -eq 1 ] && files+=("$f")
    done
    for e in "$@"; do
        case " ${all[*]} " in
            *" $e "*) echo "exempt   $e" ;;
            *)        echo "exempt   $e (no Go change)" ;;
        esac
    done
    if [ "${#files[@]}" -eq 0 ]; then
        echo "comment-budget proof: no Go file changed between $label"
        exit 0
    fi
    extract "$base" "$WORK/base"
    extract "$head" "$WORK/head"
    "$WORK/cb" prove "$WORK/base" "$WORK/head" "${files[@]}"
    rc=$?
    [ "$rc" -eq 0 ] && echo "comment-budget proof: code tokens identical in ${#files[@]} file(s)"
    exit "$rc"
}

# marker_lines: "marker" and "except <path>" for the PR_BODY lines at column 0,
# outside fenced code and HTML comments; a CRLF body is read as LF (#1056).
marker_lines() {
    printf '%s\n' "$PR_BODY" | LC_ALL=C awk '
        function fence(s,   t, c, n) {
            t = s; sub(/^(   |  | )/, "", t)
            c = substr(t, 1, 1)
            if (c != "`" && c != "~") return ""
            for (n = 1; substr(t, n + 1, 1) == c; n++) ;
            return n >= 3 ? c n : ""
        }
        { sub(/\r$/, "") }
        html { if (index($0, "-->")) html = 0; next }
        open != "" {
            f = fence($0); t = $0; sub(/^ *[`~]+[ \t]*$/, "", t)
            if (f != "" && t == "" && substr(f, 1, 1) == substr(open, 1, 1) && substr(f, 2) + 0 >= substr(open, 2) + 0) open = ""
            next
        }
        fence($0) != "" { open = fence($0); next }
        index($0, "<!--") { if (!index(substr($0, index($0, "<!--") + 4), "-->")) html = 1; next }
        /^Comments-only: yes[ \t]*$/ { print "marker"; next }
        /^Comments-only-except:/ {
            sub(/^Comments-only-except:[ \t]*/, "")
            n = split($0, a, /[ \t]+/)
            for (i = 1; i <= n; i++) if (a[i] != "") print "except " a[i]
        }'
}

if [ "${1-}" = "--prove" ]; then
    [ "$#" -eq 3 ] || die2 "usage: $0 --prove <base> <head>"
    base=$(commit_of "$2") || exit 2
    head=$(commit_of "$3") || exit 2
    prove_files "$base" "$head" "$2 and $3"
fi

if [ "${1-}" = "--prove-marked" ]; then
    # The workflow passes no revisions outside a pull request (#1056).
    if [ "$#" -eq 1 ]; then
        echo "comment-budget proof: SKIP, no pull request at this stage"
        exit 0
    fi
    [ "$#" -eq 3 ] || die2 "usage: $0 --prove-marked [<base> <head>]"
    if [ -z "${PR_BODY+set}" ]; then
        echo "comment-budget proof: SKIP, PR_BODY is not set (no pull-request body at this stage)"
        exit 0
    fi
    marked=0
    exempt=()
    while read -r kind path; do
        case "$kind" in
            marker) marked=1 ;;
            except) exempt+=("$path") ;;
        esac
    done < <(marker_lines)
    if [ "$marked" -eq 0 ]; then
        echo "comment-budget proof: SKIP, the pull-request body has no 'Comments-only: yes' line"
        exit 0
    fi
    base=$(commit_of "$2") || exit 2
    head=$(commit_of "$3") || exit 2
    # The base tip may carry code the pull request never touched (#1056).
    mb=$(git merge-base "$base" "$head" 2>/dev/null) || die2 "no merge base between $2 and $3"
    echo "comment-budget proof: 'Comments-only: yes', judging $(git rev-parse --short "$mb")..$(git rev-parse --short "$head")"
    prove_files "$mb" "$head" "the merge base and $3" "${exempt[@]}"
fi

if [ "${1-}" = "--whole" ]; then
    [ "$#" -ge 2 ] || die2 "usage: $0 --whole <rev> [<path>...]"
    rev=$(commit_of "$2") || exit 2
    shift 2
    [ "$#" -eq 0 ] && set -- .
    files=()
    for p in "$@"; do
        mapfile -t got < <(git -c core.quotePath=false ls-tree -r --name-only --full-tree "$rev" -- "$p" \
            | grep -E '\.go$' | grep -Ev '(^|/)(testdata|vendor)/')
        [ "${#got[@]}" -gt 0 ] || die2 "'$p' names no Go file at $rev"
        files+=("${got[@]}")
    done
    extract "$rev" "$WORK/head"
    "$WORK/cb" whole "$WORK/head" "${files[@]}"
    exit $?
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
