#!/usr/bin/env bash
# deprecations.sh — surface removal-eligibility from CHANGELOG.md
#
# Per ADR-0008 §Amendment B (structured ### Deprecated format), walks
# CHANGELOG.md and reports each deprecation entry's
# (deprecated-in, earliest-removal) pair. Used at release-cut time to
# answer "which surfaces are cleared for removal at v<X.Y.Z>?".
#
# Usage:
#   ./scripts/deprecations.sh --for v<X.Y.Z>   surfaces cleared at v<X.Y.Z>
#   ./scripts/deprecations.sh --all            full table, all entries
#   ./scripts/deprecations.sh --help
#
# Env:
#   CHANGELOG  path to CHANGELOG.md (default: CHANGELOG.md in cwd)

set -euo pipefail

CHANGELOG="${CHANGELOG:-CHANGELOG.md}"
mode=""
target=""

usage() {
    cat <<'EOF'
Usage:
  ./scripts/deprecations.sh --for v<X.Y.Z>   surfaces cleared at v<X.Y.Z>
  ./scripts/deprecations.sh --all            full table, all entries
  ./scripts/deprecations.sh --help

Reads $CHANGELOG (default: CHANGELOG.md) and reports each ### Deprecated
entry's (deprecated-in, earliest-removal) version pin per ADR-0008
§Amendment B.
EOF
}

while [ $# -gt 0 ]; do
    case "$1" in
        --for)
            mode="for"
            shift
            target="${1:-}"
            ;;
        --all)
            mode="all"
            ;;
        --help|-h)
            usage
            exit 0
            ;;
        *)
            echo "Unknown option: $1" >&2
            usage >&2
            exit 2
            ;;
    esac
    shift || true
done

if [ -z "$mode" ]; then
    usage >&2
    exit 2
fi

if [ "$mode" = "for" ] && [ -z "$target" ]; then
    echo "--for requires a version (e.g. --for v0.11.0)" >&2
    exit 2
fi

if [ ! -f "$CHANGELOG" ]; then
    echo "CHANGELOG not found: $CHANGELOG" >&2
    exit 1
fi

# semver_cmp A B → echo 0 if A==B, 1 if A<B, 2 if A>B.
# Strips leading 'v' from both; missing components default to 0.
semver_cmp() {
    local a="${1#v}" b="${2#v}"
    local IFS=.
    # shellcheck disable=SC2206
    local ax=($a) bx=($b)
    local i ai bi
    for i in 0 1 2; do
        ai="${ax[$i]:-0}"
        bi="${bx[$i]:-0}"
        if [ "$ai" -lt "$bi" ]; then echo 1; return; fi
        if [ "$ai" -gt "$bi" ]; then echo 2; return; fi
    done
    echo 0
}

# Extract tuples from CHANGELOG: TAB-separated
#   section, deprecated_in, earliest_removal, legacy_flag, issue_ref, surface_title
# section / deprecated_in / earliest_removal are bare versions (no leading 'v').
# legacy_flag: "1" if canonical version-pin line absent (deprecated_in inferred from section).
# issue_ref: first "#NNNN" found in entry, or "" if none.
# surface_title: stripped title text (bold markers + newlines normalized).
extract_tuples() {
    awk '
    function trim(s) { sub(/^[ \t\r\n]+/, "", s); sub(/[ \t\r\n]+$/, "", s); return s }

    function extract_title(text,   t, pos) {
        t = text
        sub(/^[ \t\r\n]*-[ \t]+\*\*/, "", t)
        if (match(t, /\.\*\*/)) {
            t = substr(t, 1, RSTART - 1)
        } else if (match(t, /\*\*/)) {
            t = substr(t, 1, RSTART - 1)
        }
        gsub(/[ \t\r\n]+/, " ", t)
        return trim(t)
    }

    function extract_version(text, prefix_re,   r, s, v) {
        r = prefix_re "[ \t\r\n*_]+v[0-9]+\\.[0-9]+\\.[0-9]+"
        if (match(text, r)) {
            s = substr(text, RSTART, RLENGTH)
            if (match(s, /v[0-9]+\.[0-9]+\.[0-9]+/)) {
                return substr(s, RSTART + 1, RLENGTH - 1)
            }
        }
        return ""
    }

    function extract_issue(text,   v) {
        if (match(text, /#[0-9]+/)) {
            return substr(text, RSTART, RLENGTH)
        }
        return ""
    }

    function flush_entry(   dep, rem, legacy, title, issue, dep_in) {
        if (!pending) return
        title = extract_title(entry_text)
        issue = extract_issue(entry_text)

        # Canonical: "Deprecated in vX.Y.Z; earliest removal vA.B.C"
        # Permissive: accept either case on both words.
        dep = extract_version(entry_text, "[Dd]eprecated in")
        rem = extract_version(entry_text, "[Ee]arliest removal")

        legacy = "0"
        dep_in = dep
        if (dep_in == "") {
            if (pending_section ~ /^[0-9]+\.[0-9]+\.[0-9]+$/) {
                dep_in = pending_section
                legacy = "1"
            }
        }

        printf "%s\037%s\037%s\037%s\037%s\037%s\n", \
            pending_section, dep_in, rem, legacy, issue, title
        pending = 0
        entry_text = ""
    }

    # Track release section
    /^## \[/ {
        flush_entry()
        match($0, /\[[^]]+\]/)
        cur_section = substr($0, RSTART + 1, RLENGTH - 2)
        sub(/^v/, "", cur_section)
        in_deprecated = 0
        next
    }

    /^### Deprecated[ \t]*$/ { flush_entry(); in_deprecated = 1; next }
    /^### / && !/^### Deprecated/ { flush_entry(); in_deprecated = 0 }
    /^## / { flush_entry(); in_deprecated = 0 }

    # Entry title line: starts with "- **" at column 0/1
    in_deprecated && /^-[ \t]+\*\*/ {
        flush_entry()
        pending = 1
        pending_section = cur_section
        entry_text = $0
        next
    }

    # Accumulate body
    pending {
        entry_text = entry_text "\n" $0
    }

    END { flush_entry() }
    ' "$CHANGELOG"
}

# Truncate to N chars
truncate() {
    local s="$1" n="$2"
    if [ "${#s}" -gt "$n" ]; then
        printf '%s…' "${s:0:n-1}"
    else
        printf '%s' "$s"
    fi
}

case "$mode" in
    all)
        printf '%-10s  %-10s  %-10s  %-7s  %s\n' \
            "section" "deprecated" "removal" "issue" "surface"
        printf '%-10s  %-10s  %-10s  %-7s  %s\n' \
            "----------" "----------" "----------" "-------" "-------"
        while IFS=$'\037' read -r section dep_in earliest legacy issue title; do
            tag=""
            [ "$legacy" = "1" ] && tag=" [legacy]"
            printf 'v%-9s v%-9s v%-9s %-7s %s%s\n' \
                "$section" "$dep_in" "${earliest:-?}" "${issue:-—}" \
                "$(truncate "$title" 70)" "$tag"
        done < <(extract_tuples)
        ;;
    for)
        tgt="${target#v}"
        cleared=""
        pending_list=""
        unpinned_list=""
        cleared_count=0
        pending_count=0
        unpinned_count=0
        while IFS=$'\037' read -r section dep_in earliest legacy issue title; do
            tag=""
            [ "$legacy" = "1" ] && tag=" [legacy format — verify manually]"
            if [ -z "$earliest" ]; then
                line=$(printf "  ⚠  %s  (section v%s%s — no version-pin extracted)" \
                    "$(truncate "$title" 60)" \
                    "$section" \
                    "${issue:+, $issue}")
                unpinned_list+="${line}"$'\n'
                unpinned_count=$((unpinned_count + 1))
                continue
            fi
            cmp=$(semver_cmp "v$earliest" "v$tgt")
            if [ "$cmp" = "0" ] || [ "$cmp" = "1" ]; then
                line=$(printf "  ✅ %s  (deprecated v%s, earliest v%s%s)%s" \
                    "$(truncate "$title" 60)" \
                    "$dep_in" "$earliest" \
                    "${issue:+, $issue}" \
                    "$tag")
                cleared+="${line}"$'\n'
                cleared_count=$((cleared_count + 1))
            else
                line=$(printf "  ⏳ %s  (deprecated v%s, earliest v%s%s)%s" \
                    "$(truncate "$title" 60)" \
                    "$dep_in" "$earliest" \
                    "${issue:+, $issue}" \
                    "$tag")
                pending_list+="${line}"$'\n'
                pending_count=$((pending_count + 1))
            fi
        done < <(extract_tuples)

        total=$((cleared_count + pending_count + unpinned_count))
        echo "v${tgt} — removal eligibility (${cleared_count} cleared, ${pending_count} pending, ${unpinned_count} unpinned; ${total} total)"
        echo ""
        if [ "$cleared_count" -gt 0 ]; then
            echo "Cleared for removal:"
            printf '%s' "$cleared"
            echo ""
        fi
        if [ "$pending_count" -gt 0 ]; then
            echo "Not yet eligible:"
            printf '%s' "$pending_list"
            echo ""
        fi
        if [ "$unpinned_count" -gt 0 ]; then
            echo "Unpinned entries (no version-pin extracted — eyeball manually):"
            printf '%s' "$unpinned_list"
        fi
        if [ "$total" = "0" ]; then
            echo "(no deprecation entries found)"
        fi
        ;;
esac
