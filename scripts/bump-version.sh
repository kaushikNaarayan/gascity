#!/usr/bin/env bash
# Version bump script for Gas City.
#
# Gas City does NOT carry a Version constant in Go source — version is injected
# into main.version at build time via ldflags (see Makefile + .goreleaser.yml).
# The git tag is the single source of truth.
#
# This script exists to move the CHANGELOG [Unreleased] section to a new
# [X.Y.Z] section, set GC_VERSION in the VERSIONS manifest, commit, tag, and
# push. install.sh reads VERSIONS AT THE TAG, so GC_VERSION must be bumped in
# the very commit the tag points at (gcf-g2czv: v1.3.18 was tagged with
# GC_VERSION=1.3.16 and install.sh refused it). BD_*/DOLT_* pins are not
# touched: they are the town's live bd/Dolt and are set by hand before the bump.
# If future channels (npm, Nix) get added, extend here.
#
# QUICK START:
#   ./scripts/bump-version.sh X.Y.Z --commit --tag --push

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/common.sh
source "$SCRIPT_DIR/lib/common.sh"

usage() {
    cat <<EOF
Usage: $0 <version> [--commit] [--tag] [--push]

Cut a Gas City release. Rewrites CHANGELOG.md and VERSIONS (GC_VERSION only;
set BD_VERSION/BD_SHA256_* to the town's live bd beforehand and commit that
first) so both land in the commit the tag points at.

Arguments:
  <version>        Semantic version (e.g., 1.0.0, 1.1.0). No leading 'v', no pre-release suffix.
  --commit         Automatically create a git commit for the CHANGELOG change.
  --tag            Create annotated git tag vX.Y.Z (requires --commit).
  --push           Push commit and tag to origin (requires --tag).

Examples:
  $0 1.0.0                          # Update CHANGELOG, show diff, stop.
  $0 1.0.0 --commit                 # Update and commit.
  $0 1.0.0 --commit --tag           # Update, commit, tag.
  $0 1.0.0 --commit --tag --push    # Full release.

After --push, GitHub Actions release.yml runs GoReleaser and publishes
the GitHub release. Once the formula is in homebrew-core on the autobump list,
BrewTestBot opens the bump PR automatically within a few hours.

See RELEASING.md for the full process.
EOF
    exit 1
}

validate_version() {
    local version=$1
    if ! [[ $version =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
        printf '%bError: version must be MAJOR.MINOR.PATCH with no prefix or suffix (got: %s)%b\n' \
            "$RED" "$version" "$NC" >&2
        exit 1
    fi
}

update_changelog() {
    local version=$1
    local date
    date=$(date +%Y-%m-%d)

    if [ ! -f "CHANGELOG.md" ]; then
        printf '%bError: CHANGELOG.md not found at repo root%b\n' "$RED" "$NC" >&2
        exit 1
    fi

    if ! grep -q "^## \[Unreleased\]" CHANGELOG.md; then
        printf '%bError: No [Unreleased] section in CHANGELOG.md%b\n' "$RED" "$NC" >&2
        exit 1
    fi

    # Insert a new "## [X.Y.Z] - DATE" header directly under "## [Unreleased]".
    # Portable across GNU and BSD sed via sed_i helper.
    sed_i "s/^## \[Unreleased\]$/## [Unreleased]\\n\\n## [$version] - $date/" CHANGELOG.md
}

# Set GC_VERSION in the VERSIONS manifest. Fails closed (before any commit or
# tag) when the file or its single GC_VERSION line is missing.
update_versions_manifest() {
    local version=$1
    if [ ! -f "VERSIONS" ]; then
        printf '%bError: VERSIONS not found at repo root%b\n' "$RED" "$NC" >&2
        exit 1
    fi
    if [ "$(grep -c '^GC_VERSION=' VERSIONS)" != 1 ]; then
        printf '%bError: VERSIONS must contain exactly one GC_VERSION= line%b\n' "$RED" "$NC" >&2
        exit 1
    fi
    sed_i "s/^GC_VERSION=.*$/GC_VERSION=$version/" VERSIONS
    if ! grep -qx "GC_VERSION=$version" VERSIONS; then
        printf '%bError: failed to set GC_VERSION=%s in VERSIONS%b\n' "$RED" "$version" "$NC" >&2
        exit 1
    fi
}

main() {
    [ $# -lt 1 ] && usage
    case "$1" in
        -h|--help) usage ;;
    esac

    local NEW_VERSION=$1
    shift
    local AUTO_COMMIT=false AUTO_TAG=false AUTO_PUSH=false

    while [ $# -gt 0 ]; do
        case "$1" in
            --commit) AUTO_COMMIT=true ;;
            --tag)    AUTO_TAG=true ;;
            --push)   AUTO_PUSH=true ;;
            -h|--help) usage ;;
            *)
                printf '%bError: unknown option %q%b\n' "$RED" "$1" "$NC" >&2
                usage
                ;;
        esac
        shift
    done

    if [ "$AUTO_TAG" = true ] && [ "$AUTO_COMMIT" = false ]; then
        printf '%bError: --tag requires --commit%b\n' "$RED" "$NC" >&2
        exit 1
    fi
    if [ "$AUTO_PUSH" = true ] && [ "$AUTO_TAG" = false ]; then
        printf '%bError: --push requires --tag%b\n' "$RED" "$NC" >&2
        exit 1
    fi

    validate_version "$NEW_VERSION"

    if [ ! -f "CHANGELOG.md" ] || [ ! -f "go.mod" ]; then
        printf '%bError: must run from repository root%b\n' "$RED" "$NC" >&2
        exit 1
    fi

    if ! git diff-index --quiet HEAD --; then
        if [ "$AUTO_COMMIT" = true ]; then
            printf '%bError: cannot auto-commit with existing uncommitted changes%b\n' \
                "$RED" "$NC" >&2
            exit 1
        fi
        printf '%bWarning: you have uncommitted changes%b\n' "$YELLOW" "$NC"
    fi

    local TAG="v$NEW_VERSION"
    if git rev-parse "$TAG" >/dev/null 2>&1; then
        printf '%bError: tag %s already exists%b\n' "$RED" "$TAG" "$NC" >&2
        exit 1
    fi

    printf '%bBumping CHANGELOG and VERSIONS to %s%b\n\n' "$YELLOW" "$NEW_VERSION" "$NC"
    # Validate VERSIONS first so a failure leaves CHANGELOG.md untouched.
    update_versions_manifest "$NEW_VERSION"
    update_changelog "$NEW_VERSION"

    printf '%b✓ CHANGELOG.md and VERSIONS updated%b\n\n' "$GREEN" "$NC"
    git diff --stat CHANGELOG.md VERSIONS
    printf 'Pinned for this release: %s\n\n' "$(grep -E '^(GC|BD)_VERSION=' VERSIONS | tr '\n' ' ')"

    if [ "$AUTO_COMMIT" = true ]; then
        git add CHANGELOG.md VERSIONS
        git commit -m "chore: release v$NEW_VERSION"
        printf '%b✓ Commit created%b\n' "$GREEN" "$NC"

        if [ "$AUTO_TAG" = true ]; then
            git tag -a "$TAG" -m "Release $TAG"
            printf '%b✓ Tag %s created%b\n' "$GREEN" "$TAG" "$NC"
        fi

        if [ "$AUTO_PUSH" = true ]; then
            git push origin HEAD
            git push origin "$TAG"
            printf '%b✓ Pushed commit and tag to origin%b\n' "$GREEN" "$NC"
            printf '\nRelease %s initiated. GitHub Actions will build artifacts in ~5-10 minutes.\n' "$TAG"
            printf 'Monitor: https://github.com/gastownhall/gascity/actions\n'
        else
            printf '\nNext steps:\n'
            [ "$AUTO_TAG" = false ] && printf '  git tag -a %s -m "Release %s"\n' "$TAG" "$TAG"
            printf '  git push origin HEAD\n'
            printf '  git push origin %s\n' "$TAG"
        fi
    else
        printf 'Review the diff above.\n\n'
        printf 'To complete the release:\n'
        printf '  %s %s --commit --tag --push\n' "$0" "$NEW_VERSION"
    fi
}

main "$@"
