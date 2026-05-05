#!/usr/bin/env bash
# scripts/migrations-lint.sh — static linter for SQLite migrations.
#
# Purpose
# -------
# Catch the bug class introduced by migrations 058/059 BEFORE it lands again:
# a migration that drops a table referenced by FKs in OTHER tables, without
# rebuilding those referrers AND without leaving a healthy table at the same
# name in the SAME migration.
#
# In SQLite (>=3.26.0) without legacy_alter_table=ON, ALTER TABLE RENAME rewrites
# every FK in sqlite_master that points at the renamed table to point at the new
# (temp) name.  When the temp table is later DROPped, those FK references become
# DANGLING.  Any subsequent INSERT into a dependent table fails with
# "no such table: _<old>_old".
#
# Detection rules
# ---------------
# Only the FINAL state matters: after the migration runs, does some live table T
# still exist?  We mark T as "destroyed by mig" iff:
#   - mig drops T, AND
#   - mig does NOT create / rename-into a fresh T in the same file
#
# Then we scan ALL OTHER migrations for tables with REFERENCES T(...).  The
# dependents must either:
#   (a) be allowlisted via scripts/migrations-lint-allowlist.txt
#   (b) themselves be dropped in the destroyer migration (cascade wipe)
#   (c) appear in a later migration that also rebuilds T (post-repair)
# Otherwise the linter reports an unrepaired dangling-FK risk.
#
# Allowlist format
# ----------------
# scripts/migrations-lint-allowlist.txt — one entry per line, "destroyer:dependent",
# e.g. "058_users_role_check.sql:103_repair_users_old_fk_dangling.sql"
#
# Comments (# ...) and blank lines are ignored.
#
# Exit codes
# ----------
#   0  — clean
#   1  — at least one unrepaired dangling-FK risk
#   2  — invalid invocation / missing files

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
MIG_DIR="${REPO_ROOT}/internal/db/migrations"
ALLOW="${REPO_ROOT}/scripts/migrations-lint-allowlist.txt"

if [[ ! -d "$MIG_DIR" ]]; then
    echo "migrations-lint: missing migrations directory: $MIG_DIR" >&2
    exit 2
fi

shopt -s nullglob
migrations=( "$MIG_DIR"/*.sql )
if [[ ${#migrations[@]} -eq 0 ]]; then
    echo "migrations-lint: no .sql files in $MIG_DIR" >&2
    exit 2
fi

declare -A ALLOWED=()
if [[ -f "$ALLOW" ]]; then
    while IFS= read -r line; do
        case "$line" in
            ''|'#'*) continue ;;
        esac
        ALLOWED["$line"]=1
    done < "$ALLOW"
fi

# strip_sql_comments — pipe stdin through, drop --line and /* block */ comments.
strip_sql_comments() {
    # Drop -- single-line comments first, then /* ... */ block comments.
    sed -E 's@--[^\n]*@@g' | tr '\n' ' ' | sed -E 's@/\*[^*]*\*+([^/*][^*]*\*+)*/@@g'
}

# extract_dropped <file>: emit names DROPped by the migration (one per line).
# Strips temp-prefix tables (those start with `_`) — they're internal to a
# rebuild idiom and never have external FK targets.
extract_dropped() {
    strip_sql_comments < "$1" \
        | grep -oEi 'DROP[[:space:]]+TABLE([[:space:]]+IF[[:space:]]+EXISTS)?[[:space:]]+"?[A-Za-z_][A-Za-z0-9_]*' \
        | sed -E 's/^DROP[[:space:]]+TABLE([[:space:]]+IF[[:space:]]+EXISTS)?[[:space:]]+"?//I' \
        | tr -d '"' \
        | grep -v '^_' || true
}

# extract_created <file>: emit names CREATEd directly by the migration.
extract_created() {
    strip_sql_comments < "$1" \
        | grep -oEi 'CREATE[[:space:]]+TABLE([[:space:]]+IF[[:space:]]+NOT[[:space:]]+EXISTS)?[[:space:]]+"?[A-Za-z_][A-Za-z0-9_]*' \
        | sed -E 's/^CREATE[[:space:]]+TABLE([[:space:]]+IF[[:space:]]+NOT[[:space:]]+EXISTS)?[[:space:]]+"?//I' \
        | tr -d '"' || true
}

# extract_renamed_to <file>: emit (target) names that some other table is RENAMEd
# INTO. Used to detect the "_new RENAME TO original" pattern.
extract_renamed_to() {
    strip_sql_comments < "$1" \
        | grep -oEi 'RENAME[[:space:]]+TO[[:space:]]+"?[A-Za-z_][A-Za-z0-9_]*' \
        | sed -E 's/^RENAME[[:space:]]+TO[[:space:]]+"?//I' \
        | tr -d '"' || true
}

# files_referencing <table>: filenames whose CREATE TABLE bodies use REFERENCES <table>(...)
files_referencing() {
    grep -lEi "REFERENCES[[:space:]]+\"?${1}\"?" "$MIG_DIR"/*.sql 2>/dev/null || true
}

# Main loop -------------------------------------------------------------------

violations=0

for mig in "${migrations[@]}"; do
    mig_base="$(basename "$mig")"

    dropped="$(extract_dropped "$mig" | sort -u)"
    [[ -z "$dropped" ]] && continue

    created="$(extract_created "$mig" | sort -u)"
    renamed_to="$(extract_renamed_to "$mig" | sort -u)"

    # A drop is "destruction" iff the same migration leaves no fresh table by
    # the same name.
    while IFS= read -r target; do
        [[ -z "$target" ]] && continue

        if echo "$created" | grep -qx -- "$target"; then
            continue  # rebuilt via direct CREATE TABLE
        fi
        if echo "$renamed_to" | grep -qx -- "$target"; then
            continue  # rebuilt via _new RENAME TO original
        fi

        # Real destruction. Find dependents.
        deps="$(files_referencing "$target" || true)"
        for dep in $deps; do
            dep_base="$(basename "$dep")"
            [[ "$dep_base" == "$mig_base" ]] && continue

            # If the dep file itself drops target, it's a coordinated wipe.
            if extract_dropped "$dep" | grep -qx -- "$target"; then
                continue
            fi

            # Allowlist?
            allow_key="${mig_base}:${dep_base}"
            if [[ -n "${ALLOWED[$allow_key]:-}" ]]; then
                continue
            fi

            # Look for a later migration that rebuilds target (CREATE TABLE or
            # RENAME TO target) — i.e. the dangling FK gets repaired downstream.
            rebuilt_by=""
            for between in "${migrations[@]}"; do
                bbase="$(basename "$between")"
                [[ "$bbase" > "$mig_base" ]] || continue
                if extract_created "$between" | grep -qx -- "$target" \
                   || extract_renamed_to "$between" | grep -qx -- "$target"; then
                    rebuilt_by="$bbase"
                    break
                fi
            done
            if [[ -n "$rebuilt_by" ]]; then
                continue
            fi

            echo "migrations-lint: ${mig_base} destroys table '${target}' but ${dep_base} references it without coordinated repair"
            violations=$((violations + 1))
        done
    done <<< "$dropped"
done

if [[ $violations -gt 0 ]]; then
    echo ""
    echo "migrations-lint: ${violations} unrepaired dangling-FK risk(s)"
    echo "fix: rebuild the dependent table in the same migration that destroys the parent,"
    echo "     OR add a repair migration and an entry to scripts/migrations-lint-allowlist.txt"
    exit 1
fi

echo "migrations-lint: clean (${#migrations[@]} migrations checked)"
exit 0
