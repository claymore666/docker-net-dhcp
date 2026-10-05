#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only
#
# Meta-test for check-breaking-names.sh (#856).
#
# The subject is the difference between two trees, so every fixture is a small
# git repository: a base commit tagged v1.0.0, then a head commit that drops,
# renames or adds names, with a RELEASE_NOTES.md that does or does not name
# them. Each red case is paired with its green twin: a removed name absent from
# the notes fails, the same name in backticks passes, and an added name never
# counts. The refusals (no tag, a source with no names, a heading that is not
# there) are cases too, and so is the real repository, which must be judged
# (exit 0 or 1), never refused.
# shellcheck disable=SC2016  # backticks are the notes' own markup, not expansions
set -uo pipefail

# shellcheck source=scripts/tmpdir-guard.sh
. "$(cd "$(dirname "$0")" && pwd)/tmpdir-guard.sh"

HERE="$(cd "$(dirname "$0")" && pwd)"
REPO="$(cd "$HERE/.." && pwd)"
GATE="$HERE/check-breaking-names.sh"

guarded_tmpdir TMP
cp "$HERE/gatelib.sh" "$TMP/"
pass=0
fail=0

ok() { printf 'PASS  %s\n' "$1"; pass=$((pass + 1)); }
no() { printf 'FAIL  %s\n' "$1" >&2; fail=$((fail + 1)); }

G() { git -C "$1" -c user.name=t -c user.email=t@t -c commit.gpgsign=false -c tag.gpgsign=false "${@:2}"; }

BM="net_dhcp_a_total net_dhcp_b_total net_dhcp_c_total"
BK="status alpha beta gamma"
BC="bound stopped renewed"

# src <dir> <metrics> <keys> <kinds>: the three surfaces, one name per word.
src() {
    local d="$1" n i=0
    mkdir -p "$d/pkg/plugin/testdata"
    : > "$d/pkg/plugin/testdata/metrics_exposition.golden"
    for n in $2; do
        printf '# HELP %s help text\n# TYPE %s counter\n%s 1\n\n' "$n" "$n" "$n" >> "$d/pkg/plugin/testdata/metrics_exposition.golden"
    done
    {
        printf 'package plugin\n\n// HealthResponse is the document.\ntype HealthResponse struct {\n'
        for n in $3; do
            i=$((i + 1))
            printf '\t// F%d comment, json:"not_a_key" stays a comment.\n\tF%d int `json:"%s"`\n' "$i" "$i" "$n"
        done
        printf '}\n\ntype other struct {\n\tX int `json:"x_other"`\n}\n'
    } > "$d/pkg/plugin/endpoints.go"
    {
        printf 'package plugin\n\ntype dhcpManager struct{}\n\n'
        printf '// audit forwards.\nfunc (m *dhcpManager) audit(kind, ip string) {\n\tm.auditFrom(kind, ip, "")\n}\n\n'
        printf 'func (m *dhcpManager) auditFrom(kind, ip, source string) {}\n\n'
        printf 'func (m *dhcpManager) run() {\n'
        for n in $4; do
            printf '\tm.audit("%s", "")\n' "$n"
        done
        printf '\tm.auditFrom("%s", "", "src")\n}\n' "${4%% *}"
    } > "$d/pkg/plugin/dhcp_manager.go"
}

# notes <dir> <text above the v1.0.0 heading> [<inside v1.0.0>] [<below>]
notes() {
    {
        printf '# Release notes\n\nIntro text.\n\n'
        printf '%s' "$2"
        printf '## v1.0.0\n\nFirst release.\n%s\n\n' "${3:-}"
        printf '## Known limitations\n\nLimits.\n%s\n' "${4:-}"
    } > "$1/RELEASE_NOTES.md"
}

commit() { G "$1" add -A && G "$1" commit -q -m "$2"; }

# base <dir> [metrics keys kinds]: init, the v1.0.0 tree, tagged.
base() {
    git init -q "$1"
    src "$1" "${2-$BM}" "${3-$BK}" "${4-$BC}"
    notes "$1" ""
    commit "$1" base
    G "$1" tag v1.0.0
}

# head <dir> <metrics> <keys> <kinds> <text above v1.0.0>
head_() {
    src "$1" "$2" "$3" "$4"
    notes "$1" "$5"
    commit "$1" head
}

B_NAMED=$'## v1.1.0\n\nThe `net_dhcp_b_total` series is removed.\n\n'
B_SILENT=$'## v1.1.0\n\nSomething else changed.\n\n'
NO_B="net_dhcp_a_total net_dhcp_c_total"

fx_removed_named() { base "$1"; head_ "$1" "$NO_B" "$BK" "$BC" "$B_NAMED"; }
fx_removed_unnamed() { base "$1"; head_ "$1" "$NO_B" "$BK" "$BC" "$B_SILENT"; }
fx_removed_labelled() {
    base "$1"
    head_ "$1" "$NO_B" "$BK" "$BC" $'## v1.1.0\n\nDrop `net_dhcp_b_total{family="ipv4"}` from dashboards.\n\n'
}
fx_renamed_old_named() {
    base "$1"
    head_ "$1" "net_dhcp_a_total net_dhcp_b2_total net_dhcp_c_total" "$BK" "$BC" \
        $'## v1.1.0\n\n`net_dhcp_b_total` is now `net_dhcp_b2_total`.\n\n'
}
fx_renamed_new_named_only() {
    base "$1"
    head_ "$1" "net_dhcp_a_total net_dhcp_b2_total net_dhcp_c_total" "$BK" "$BC" \
        $'## v1.1.0\n\nA new series, `net_dhcp_b2_total`, appears.\n\n'
}
fx_added_only_section() {
    base "$1"
    head_ "$1" "$BM net_dhcp_new_total" "$BK newkey" "$BC newkind" "$B_SILENT"
}
fx_added_only_no_section() {
    base "$1"
    src "$1" "$BM net_dhcp_new_total" "$BK newkey" "$BC newkind"
    commit "$1" add
}
fx_health_removed_named() { base "$1"; head_ "$1" "$BM" "status alpha gamma" "$BC" $'## v1.1.0\n\nThe health key `beta` is gone.\n\n'; }
fx_health_removed_unnamed() { base "$1"; head_ "$1" "$BM" "status alpha gamma" "$BC" "$B_SILENT"; }
fx_kind_removed_named() { base "$1"; head_ "$1" "$BM" "$BK" "bound renewed" $'## v1.1.0\n\nThe ledger kind `stopped` is no longer written.\n\n'; }
fx_kind_removed_unnamed() { base "$1"; head_ "$1" "$BM" "$BK" "bound renewed" "$B_SILENT"; }
fx_other_struct_tag_removed() {
    base "$1"
    head_ "$1" "$BM" "$BK" "$BC" "$B_SILENT"
    sed -i '/x_other/d' "$1/pkg/plugin/endpoints.go"
    commit "$1" drop
}
fx_test_file_kind_removed() {
    base "$1"
    printf 'package plugin\n\nfunc t() {\n\tm.audit("onlytest", "")\n}\n' > "$1/pkg/plugin/x_test.go"
    commit "$1" addtest
    G "$1" tag -f v1.0.0 >/dev/null
    G "$1" rm -q pkg/plugin/x_test.go
    commit "$1" rmtest
}

fx_testdata_kind_removed() {
    base "$1"
    mkdir -p "$1/pkg/plugin/testdata"
    printf 'package plugin\n\nfunc t() {\n\tm.audit("onlytestdata", "")\n}\n' > "$1/pkg/plugin/testdata/x.go"
    commit "$1" adddata
    G "$1" tag -f v1.0.0 >/dev/null
    G "$1" rm -q pkg/plugin/testdata/x.go
    commit "$1" rmdata
}
fx_comment_mentions_audit() {
    base "$1"
    printf '\n// A comment may say m.audit(kind, ip) or audit(\"x\" without being a call.\n' >> "$1/pkg/plugin/dhcp_manager.go"
    commit "$1" comment
    G "$1" tag -f v1.0.0 >/dev/null
    head_ "$1" "$BM" "$BK" "$BC" "$B_SILENT"
}

fx_no_section_empty_set() { base "$1"; }
fx_no_section_nonempty() {
    base "$1"
    src "$1" "$NO_B" "$BK" "$BC"
    commit "$1" drop
}
fx_section_empty_set() { base "$1"; head_ "$1" "$BM" "$BK" "$BC" "$B_SILENT"; }

fx_no_tag() {
    git init -q "$1"
    src "$1" "$BM" "$BK" "$BC"
    notes "$1" ""
    commit "$1" base
}
fx_only_rc_tag() {
    git init -q "$1"
    src "$1" "$BM" "$BK" "$BC"
    notes "$1" ""
    commit "$1" base
    G "$1" tag v1.0.0-rc1
}
fx_prev_heading_missing() {
    base "$1"
    head_ "$1" "$NO_B" "$BK" "$BC" "$B_NAMED"
    sed -i 's/^## v1.0.0$/## First release/' "$1/RELEASE_NOTES.md"
    commit "$1" rename
}
fx_notes_missing() {
    base "$1"
    G "$1" rm -q RELEASE_NOTES.md
    commit "$1" rm
}

fx_named_only_in_prev_section() {
    base "$1" "$BM" "$BK" "$BC"
    head_ "$1" "$NO_B" "$BK" "$BC" "$B_SILENT"
    notes "$1" "$B_SILENT" $'Mentions `net_dhcp_b_total` in the released section.'
    commit "$1" notes
}
fx_named_only_below() {
    base "$1"
    head_ "$1" "$NO_B" "$BK" "$BC" "$B_SILENT"
    notes "$1" "$B_SILENT" "" $'Mentions `net_dhcp_b_total` under limitations.'
    commit "$1" notes
}
fx_named_in_second_unreleased_section() {
    base "$1"
    head_ "$1" "$NO_B" "$BK" "$BC" "$B_SILENT"$'## v1.0.1\n\nThe `net_dhcp_b_total` series is removed.\n\n'
}
fx_prose_only() {
    base "$1"
    head_ "$1" "$BM" "$BK" "bound renewed" $'## v1.1.0\n\nA stopped container keeps its address.\n\n'
}
fx_substring_only() {
    base "$1"
    head_ "$1" "$BM" "status beta gamma" "$BC" $'## v1.1.0\n\nThe key `alpha_v4` is gone and `alphabet` is not a key.\n\n'
}
fx_wrapped_span() {
    base "$1"
    head_ "$1" "$BM" "$BK" "bound renewed" $'## v1.1.0\n\nThe `kind:\nstopped` row is no longer written.\n\n'
}
fx_fenced() {
    base "$1"
    head_ "$1" "$NO_B" "$BK" "$BC" $'## v1.1.0\n\nRemoved:\n\n```\nnet_dhcp_b_total\n```\n\n'
}
fx_prose_after_fence() {
    base "$1"
    head_ "$1" "$NO_B" "$BK" "$BC" $'## v1.1.0\n\n```\nsomething else\n```\n\nThe net_dhcp_b_total series is removed.\n\n'
}

fx_rc_newer_than_stable() {
    base "$1"
    head_ "$1" "$NO_B" "$BK" "$BC" "$B_SILENT"
    G "$1" tag v1.1.0-rc1
}
fx_version_order() {
    git init -q "$1"
    src "$1" "$BM" "$BK" "$BC"
    printf '# Release notes\n\n## v1.9.0\n\nNine.\n' > "$1/RELEASE_NOTES.md"
    commit "$1" nine
    G "$1" tag v1.9.0
    src "$1" "$NO_B" "$BK" "$BC"
    printf '# Release notes\n\n## v1.10.0\n\nTen changed something else.\n\n## v1.9.0\n\nNine.\n' > "$1/RELEASE_NOTES.md"
    commit "$1" ten
    G "$1" tag v1.10.0
}
fx_unreachable_higher_tag() {
    base "$1"
    head_ "$1" "$NO_B" "$BK" "$BC" "$B_SILENT"$'## v9.0.0\n\nNine.\n\n'
    local br
    br=$(G "$1" rev-parse --abbrev-ref HEAD)
    G "$1" checkout -q --orphan side
    commit "$1" side
    G "$1" tag v9.0.0
    G "$1" checkout -q -f "$br"
}
fx_shallow_clone() {
    local s="$1.src"
    fx_removed_unnamed "$s"
    git clone -q --depth=1 "file://$s" "$1" 2>/dev/null
    G "$1" fetch -q --tags --depth=1 origin
}
fx_shallow_clone_named() {
    local s="$1.src"
    fx_removed_named "$s"
    git clone -q --depth=1 "file://$s" "$1" 2>/dev/null
    G "$1" fetch -q --tags --depth=1 origin
}
fx_shallow_no_tags() {
    local s="$1.src"
    fx_removed_unnamed "$s"
    git clone -q --depth=1 "file://$s" "$1" 2>/dev/null
}

fx_zero_metrics_head() { base "$1"; head_ "$1" "" "$BK" "$BC" "$B_SILENT"; }
fx_zero_keys_head() { base "$1"; head_ "$1" "$BM" "" "$BC" "$B_SILENT"; }
fx_zero_kinds_head() {
    base "$1"
    head_ "$1" "$BM" "$BK" "" "$B_SILENT"
    printf 'package plugin\n' > "$1/pkg/plugin/dhcp_manager.go"
    commit "$1" nokinds
}
fx_zero_metrics_tag() { base "$1" "" "$BK" "$BC"; head_ "$1" "$BM" "$BK" "$BC" "$B_SILENT"; }
fx_zero_keys_tag() { base "$1" "$BM" "" "$BC"; head_ "$1" "$BM" "$BK" "$BC" "$B_SILENT"; }
fx_zero_kinds_tag() {
    git init -q "$1"
    src "$1" "$BM" "$BK" "$BC"
    printf 'package plugin\n' > "$1/pkg/plugin/dhcp_manager.go"
    notes "$1" ""
    commit "$1" base
    G "$1" tag v1.0.0
    head_ "$1" "$BM" "$BK" "$BC" "$B_SILENT"
}
fx_golden_moved() {
    base "$1"
    head_ "$1" "$NO_B" "$BK" "$BC" "$B_NAMED"
    G "$1" mv pkg/plugin/testdata/metrics_exposition.golden pkg/plugin/testdata/moved.golden
    commit "$1" move
}
fx_nonliteral_audit_head() {
    base "$1"
    head_ "$1" "$BM" "$BK" "$BC" "$B_SILENT"
    printf 'func (m *dhcpManager) more(k string) {\n\tm.audit(k, "")\n}\n' >> "$1/pkg/plugin/dhcp_manager.go"
    commit "$1" nonliteral
}
fx_nonliteral_audit_tag() {
    git init -q "$1"
    src "$1" "$BM" "$BK" "$BC"
    printf 'func (m *dhcpManager) more(k string) {\n\tm.auditFrom(k, "", "")\n}\n' >> "$1/pkg/plugin/dhcp_manager.go"
    notes "$1" ""
    commit "$1" base
    G "$1" tag v1.0.0
    head_ "$1" "$BM" "$BK" "$BC" "$B_SILENT"
}
fx_untagged_health_field() {
    base "$1"
    head_ "$1" "$BM" "$BK" "$BC" "$B_SILENT"
    sed -i 's/^type other struct/type Embedded struct{}\n\ntype other struct/;s/^\tF1 int `json:"status"`$/\tEmbedded\n\tF1 int `json:"status"`/' "$1/pkg/plugin/endpoints.go"
    commit "$1" untagged
}
fx_dash_field_dropped() {
    base "$1" "$BM" "$BK -" "$BC"
    head_ "$1" "$BM" "$BK" "$BC" "$B_SILENT"
}
fx_json_options() {
    base "$1"
    head_ "$1" "$BM" "$BK" "$BC" "$B_SILENT"
    sed -i 's/`json:"beta"`/`json:"beta,omitempty"`/' "$1/pkg/plugin/endpoints.go"
    commit "$1" omitempty
}
fx_json_dash() {
    base "$1"
    head_ "$1" "$BM" "$BK" "$BC" "$B_SILENT"
    sed -i 's/`json:"beta"`/`json:"-"`/' "$1/pkg/plugin/endpoints.go"
    commit "$1" dash
}

fx_h2_above_first_version() {
    base "$1"
    head_ "$1" "$NO_B" "$BK" "$BC" $'## Upgrade notes\n\nThe `net_dhcp_b_total` series is removed.\n\n## v1.1.0\n\nSomething else changed.\n\n'
}
fx_rc_section_above_prev() {
    base "$1"
    head_ "$1" "$NO_B" "$BK" "$BC" $'## v1.0.0-rc1\n\nThe `net_dhcp_b_total` series is removed.\n\n'
}
fx_fenced_in_list_item() {
    base "$1"
    head_ "$1" "$NO_B" "$BK" "$BC" $'## v1.1.0\n\nRemoved:\n\n- the series:\n\n  ```\n  a first line\n\n  net_dhcp_b_total\n  ```\n\n'
}
fx_tilde_fence() {
    base "$1"
    head_ "$1" "$NO_B" "$BK" "$BC" $'## v1.1.0\n\nRemoved:\n\n~~~\nnet_dhcp_b_total\n~~~\n\n'
}
fx_lone_backtick() {
    base "$1"
    head_ "$1" "$BM" "status alpha gamma" "$BC" $'## v1.1.0\n\nUse a single ` to quote. Check the beta first, and `docker info`.\n\n'
}
fx_lone_backtick_other_paragraph() {
    base "$1"
    head_ "$1" "$NO_B" "$BK" "$BC" $'## v1.1.0\n\nA lone ` here.\n\nThe `net_dhcp_b_total` series is removed.\n\n'
}
fx_lone_backticks_two_paragraphs() {
    base "$1"
    head_ "$1" "$NO_B" "$BK" "$BC" $'## v1.1.0\n\nA lone ` here.\n\nAnother lone ` there.\n\nThe `net_dhcp_b_total` series is removed.\n\n'
}
fx_prose_beside_span() {
    base "$1"
    head_ "$1" "$BM" "status gamma" "$BC" $'## v1.1.0\n\nThe key `beta` is removed and alpha too.\n\n'
}
fx_rc_tag_with_heading() {
    base "$1"
    head_ "$1" "$NO_B" "$BK" "$BC" $'## v1.1.0-rc1\n\nThe `net_dhcp_b_total` series is removed.\n\n'
    G "$1" tag v1.1.0-rc1 HEAD~1
}
fx_double_span_with_inner_tick() {
    base "$1"
    head_ "$1" "$NO_B" "$BK" "$BC" $'## v1.1.0\n\nThe ``net_dhcp_b_total ` series`` is removed.\n\n'
}
fx_notes_start_with_heading() {
    base "$1"
    src "$1" "$NO_B" "$BK" "$BC"
    printf '## v1.0.0\n\nFirst release.\n' > "$1/RELEASE_NOTES.md"
    commit "$1" drop
}
fx_double_backtick_span() {
    base "$1"
    head_ "$1" "$NO_B" "$BK" "$BC" $'## v1.1.0\n\nThe ``net_dhcp_b_total`` series is removed.\n\n'
}
# wrap_auditfrom <dir>: auditFrom forwards to audit with a variable, the shape of dhcp_manager.go.
wrap_auditfrom() {
    sed -i 's/^func (m \*dhcpManager) auditFrom(kind, ip, source string) {}$/func (m *dhcpManager) auditFrom(kind, ip, source string) {\n\tm.audit(kind, ip)\n}/' "$1/pkg/plugin/dhcp_manager.go"
    commit "$1" wrap
}
fx_auditfrom_wrapper_at_tag() {
    base "$1"
    wrap_auditfrom "$1"
    G "$1" tag -f v1.0.0 >/dev/null
    head_ "$1" "$BM" "$BK" "$BC" "$B_SILENT"
}
fx_auditfrom_wrapper_at_head() {
    base "$1"
    head_ "$1" "$BM" "$BK" "$BC" "$B_SILENT"
    wrap_auditfrom "$1"
}
# hotfix_clone <builder> <dir>: dir is a depth-1 clone of a tree whose newer release tag sits on a branch not merged in.
hotfix_clone() {
    local s="$2.src" br
    "$1" "$s"
    br=$(G "$s" rev-parse --abbrev-ref HEAD)
    G "$s" checkout -q -b hotfix v1.0.0
    printf 'fix\n' > "$s/hotfix.txt"
    commit "$s" hotfix
    G "$s" tag v1.0.1
    G "$s" checkout -q "$br"
    git clone -q --depth=1 "file://$s" "$2" 2>/dev/null
    G "$2" fetch -q --tags --depth=1 origin
}
fx_hotfix_unmerged_shallow_named() { hotfix_clone fx_removed_named "$1"; }
fx_hotfix_unmerged_shallow_unnamed() { hotfix_clone fx_removed_unnamed "$1"; }

CASES=(
    "a removed metric named in backticks passes (the direction trap)|fx_removed_named|0"
    "a removed metric absent from the section fails|fx_removed_unnamed|1"
    "a removed metric named with its label set passes|fx_removed_labelled|0"
    "a renamed metric whose old name is in the notes passes|fx_renamed_old_named|0"
    "a renamed metric with only the new name in the notes fails|fx_renamed_new_named_only|1"
    "additions the notes never mention pass, with a section|fx_added_only_section|0"
    "additions the notes never mention pass, with no section|fx_added_only_no_section|0"
    "a removed health key named passes|fx_health_removed_named|0"
    "a removed health key absent fails|fx_health_removed_unnamed|1"
    "a removed ledger kind named passes|fx_kind_removed_named|0"
    "a removed ledger kind absent fails|fx_kind_removed_unnamed|1"
    "a tag dropped from another struct is not a health key|fx_other_struct_tag_removed|0"
    "a kind that only a test file wrote is not a ledger kind|fx_test_file_kind_removed|0"
    "a kind that only a testdata Go file wrote is not a ledger kind|fx_testdata_kind_removed|0"
    "a comment that mentions audit( is not a call|fx_comment_mentions_audit|0"
    "dropping a json:\"-\" field changes no key|fx_dash_field_dropped|0"
    "a json tag with omitempty is the key before the comma|fx_json_options|0"
    "a json:\"-\" field removes the key and is found|fx_json_dash|1"
    "no change and no section passes|fx_no_section_empty_set|0"
    "a change and no section above the previous release fails|fx_no_section_nonempty|1"
    "no change with a section that names nothing passes|fx_section_empty_set|0"
    "a heading that is not a version above the first one is not unreleased|fx_h2_above_first_version|1"
    "a release candidate section above the previous release counts as unreleased|fx_rc_section_above_prev|0"
    "a name in an indented fenced block in a list item counts|fx_fenced_in_list_item|0"
    "a name in a tilde fence counts|fx_tilde_fence|0"
    "a double-backtick span counts|fx_double_backtick_span|0"
    "a name beside a span in the same paragraph is still prose|fx_prose_beside_span|1"
    "a release candidate tag with its own heading is not the previous release|fx_rc_tag_with_heading|0"
    "a double-backtick span may hold a single backtick|fx_double_span_with_inner_tick|0"
    "a notes file that opens with the previous release's heading has no section|fx_notes_start_with_heading|1"
    "a lone backtick does not turn prose into a span|fx_lone_backtick|1"
    "a lone backtick in a paragraph of the notes fails even with the name spelled out|fx_lone_backtick_other_paragraph|1"
    "lone backticks in two paragraphs do not pair across the blank line|fx_lone_backticks_two_paragraphs|1"
    "an auditFrom wrapper that forwards a variable is not a kind at the tag|fx_auditfrom_wrapper_at_tag|0"
    "an auditFrom wrapper that forwards a variable is not a kind at HEAD|fx_auditfrom_wrapper_at_head|0"
    "a release tag not merged back is skipped, and the name is judged against the one before it|fx_hotfix_unmerged_shallow_named|0"
    "a skipped tag does not excuse an unnamed removal|fx_hotfix_unmerged_shallow_unnamed|1"
    "no stable tag is refused|fx_no_tag|2"
    "only a release candidate tag is refused|fx_only_rc_tag|2"
    "the previous tag's heading missing from the notes is refused|fx_prev_heading_missing|2"
    "a tree with no RELEASE_NOTES.md is refused|fx_notes_missing|2"
    "a name only in the previous release's section fails|fx_named_only_in_prev_section|1"
    "a name only below the sections fails|fx_named_only_below|1"
    "a name in a second unreleased section passes|fx_named_in_second_unreleased_section|0"
    "a word in plain prose is not a mention|fx_prose_only|1"
    "a longer name and a longer word are not the name|fx_substring_only|1"
    "a backtick span wrapped over a line break counts|fx_wrapped_span|0"
    "a name in a fenced block counts|fx_fenced|0"
    "a name in prose after a closed fence does not count|fx_prose_after_fence|1"
    "a newer release candidate is not the previous release|fx_rc_newer_than_stable|1"
    "v1.10.0 is above v1.9.0|fx_version_order|0"
    "a higher tag that is not an ancestor is skipped|fx_unreachable_higher_tag|1"
    "a depth-1 clone with tags fetched fails the same way|fx_shallow_clone|1"
    "a depth-1 clone with tags fetched passes the same way|fx_shallow_clone_named|0"
    "a depth-1 clone without its tags is refused|fx_shallow_no_tags|2"
    "no metric names at HEAD is refused|fx_zero_metrics_head|2"
    "no health keys at HEAD is refused|fx_zero_keys_head|2"
    "no ledger kinds at HEAD is refused|fx_zero_kinds_head|2"
    "no metric names at the tag is refused|fx_zero_metrics_tag|2"
    "no health keys at the tag is refused|fx_zero_keys_tag|2"
    "no ledger kinds at the tag is refused|fx_zero_kinds_tag|2"
    "a moved golden file is refused|fx_golden_moved|2"
    "an audit call with a variable kind at HEAD is refused|fx_nonliteral_audit_head|2"
    "an audit call with a variable kind at the tag is refused|fx_nonliteral_audit_tag|2"
    "a HealthResponse field with no json tag is refused|fx_untagged_health_field|2"
)

# verdict <gate> <builder> -> exit code
verdict() {
    local gate="$1" builder="$2" d rc
    guarded_tmpdir d "$TMP/fx.XXXXXX"
    "$builder" "$d" >/dev/null 2>&1
    bash "$gate" "$d" >/dev/null 2>&1
    rc=$?
    echo "$rc"
}

# report <builder> -> the gate's stdout and stderr
report() {
    local d
    guarded_tmpdir d "$TMP/fx.XXXXXX"
    "$1" "$d" >/dev/null 2>&1
    bash "$GATE" "$d" 2>&1
}

echo "--- the gate itself ---"
for c in "${CASES[@]}"; do
    IFS='|' read -r desc builder want <<< "$c"
    got=$(verdict "$GATE" "$builder")
    if [ "$got" = "$want" ]; then ok "$desc"; else no "$desc: want exit $want, got $got"; fi
done

# The real repository is judged, not refused: its three sources parse at the
# previous release and at this tree. Which way it is judged depends on the
# branch, so only a refusal is wrong.
bash "$GATE" "$REPO" >/dev/null 2>&1
rc=$?
if [ "$rc" = 0 ] || [ "$rc" = 1 ]; then
    ok "the repository is judged (exit $rc), not refused"
else
    no "the repository is refused (exit $rc); the three sources must parse at the previous release and here"
fi

echo "--- what the report says ---"
check_out() {
    local desc="$1" builder="$2" pat="$3" out
    out=$(report "$builder")
    if printf '%s\n' "$out" | grep -E -- "$pat" >/dev/null; then ok "$desc"; else no "$desc: no /$pat/ in: $out"; fi
}
check_out "an empty changed set is printed as changed: 0" fx_section_empty_set '^changed: 0$'
check_out "an empty changed set with no section is printed as changed: 0" fx_no_section_empty_set '^changed: 0$'
check_out "a changed set is printed with its size" fx_removed_unnamed '^changed: 1$'
check_out "a changed set counts every surface" fx_kind_removed_unnamed '^  ledger kinds: 3 at v1.0.0, 2 at HEAD, 1 changed$'
check_out "the missing name is listed with its surface" fx_removed_unnamed 'metric name net_dhcp_b_total'
check_out "no section says a section must be added" fx_no_section_nonempty 'Add the section for the next release'
check_out "an addition is not listed" fx_added_only_section '^changed: 0$'
check_out "a skipped tag is named with the reason" fx_hotfix_unmerged_shallow_named 'skipping v1\.0\.1: no `## v1\.0\.1` heading'
check_out "a lone backtick is named by line" fx_lone_backtick '^  line 7: Use a single'
check_out "the previous release is named" fx_removed_named 'previous release v1\.0\.0 '

echo "--- mutants ---"
mutant() {
    local path="$TMP/mutant-$1.sh"
    printf '#!/usr/bin/env bash\nexit %s\n' "$1" > "$path"
    echo "$path"
}
for code in 0 1; do
    m=$(mutant "$code")
    killed=0
    for c in "${CASES[@]}"; do
        IFS='|' read -r desc builder want <<< "$c"
        [ "$(verdict "$m" "$builder")" = "$want" ] || killed=$((killed + 1))
    done
    if [ "$killed" -gt 0 ]; then
        ok "the always-exit-$code mutant is killed by $killed of ${#CASES[@]} cases"
    else
        no "the always-exit-$code mutant survives every case"
    fi
done

echo
if [ "$fail" -gt 0 ]; then
    echo "check-breaking-names meta-test: $pass passed, $fail FAILED" >&2
    exit 1
fi
echo "check-breaking-names meta-test: $pass passed, 0 failed"
