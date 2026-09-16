#!/usr/bin/env bash
# Install a pinned Gas City release and its runtime data-plane dependencies.
#
# material-lab-io/gascity is a PRIVATE repo (gcf-aetk): a plain unauthenticated
# fetch of this script -- or of anything else in that repo -- 404s. Use the gh
# CLI (already authenticated) to pull the script itself, then run it normally:
#   gh api -H 'Accept: application/vnd.github.raw' \
#     repos/material-lab-io/gascity/contents/install.sh?ref=main > install.sh
#   bash install.sh 1.3.8
# Once running, install.sh authenticates its OWN downloads the same way --
# see "Dependency pins" below.
set -euo pipefail

repo="${GC_INSTALL_REPO:-material-lab-io/gascity}"
version="${1:-${GC_VERSION:-}}"

usage() {
  cat <<'EOF'
Usage: install.sh [VERSION]

Installs gc VERSION and a caller-supplied, exact Dolt/bd release tuple. VERSION
may also be supplied through GC_VERSION. Existing binaries are replaced only
after every archive has downloaded, passed SHA-256 verification, unpacked, and
passed a smoke test (each new binary must run its own version command).

Dependency pins: GC_BD_VERSION/GC_BD_SHA256, and on Linux also
GC_DOLT_VERSION/GC_DOLT_SHA256. When any of these is not set explicitly, the
installer fetches the published VERSIONS manifest for this exact gc tag and
reads the matching value(s) for the detected OS/arch from it; an explicit
environment value always wins over whatever the manifest says. If the
manifest cannot be fetched, does not match this tag, or is missing a
required key, the installer fails closed rather than guessing. This
installer deliberately has no baked-in dependency defaults: the release
manager owns the published gc/Dolt/bd compatibility tuple, either by hand
via these variables or through the manifest they publish alongside the tag.

On darwin, Dolt is intentionally NOT installed here: the Mac's live Dolt is
Homebrew-owned. The installer names Homebrew as the owner and refuses to
touch it; use `brew upgrade dolt` there instead.

GC_INSTALL_DIR is required whenever a gc already resolves on PATH: this
installer refuses to guess whether that live toolchain should be updated. Set
GC_INSTALL_DIR to the live directory to update it explicitly, or to a
different directory for a side-by-side install. Only when no gc is found on
PATH does it fall back to ~/.local/bin.

On success, the previous gc/dolt/bd binaries are kept beside the new ones as
<binary>.prev-<old-version> for a manual revert.

GC_INSTALL_REPO defaults to a PRIVATE repo (material-lab-io/gascity): every
fetch from it (the VERSIONS manifest, gc's own release assets) needs one of,
tried in order: the gh CLI already authenticated (`gh auth status`); a
GH_TOKEN or GITHUB_TOKEN env var; or -- as a last-resort fallback, only
useful if GC_INSTALL_REPO happens to be public -- a plain unauthenticated
request. With none of those, the installer fails closed rather than
guessing. Dolt (dolthub/dolt) and bd (gastownhall/beads) are public and
always fetched unauthenticated.

Optional environment: GC_INSTALL_REPO, GC_MANIFEST_URL (advanced/test-only
override for where the VERSIONS manifest is fetched from; defaults to the
raw VERSIONS file published at this tag in GC_INSTALL_REPO), GH_TOKEN /
GITHUB_TOKEN (authenticate GC_INSTALL_REPO fetches without the gh CLI).
EOF
}

if [[ "${version}" == "-h" || "${version}" == "--help" ]]; then usage; exit 0; fi
if [[ -z "$version" ]]; then
  echo "A release version is required (for example: install.sh 1.3.7)." >&2
  exit 2
fi
version="${version#v}"
if [[ ! "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+([-.][0-9A-Za-z.]+)?$ ]]; then
  echo "Invalid release version: $version" >&2
  exit 2
fi

# install_dir: never guess at a live toolchain. Defaulting to "wherever gc
# already runs" is exactly the failure this installer must not repeat
# (gcf-vxqx, 2026-09-08: a default-path run overwrote this host's live
# gc/dolt/bd with test stubs and reported success). Require an explicit
# GC_INSTALL_DIR whenever a live gc exists; only fall back silently when
# there is nothing live to silently hit.
if [[ -n "${GC_INSTALL_DIR:-}" ]]; then
  install_dir="$GC_INSTALL_DIR"
elif live_gc="$(command -v gc 2>/dev/null)"; then
  live_dir="$(cd "$(dirname "$live_gc")" && pwd)"
  cat >&2 <<MSG
Refusing to install: a live gc is already on PATH at ${live_gc}.
This installer will not silently guess whether to update it. Re-run with:
  GC_INSTALL_DIR=${live_dir} $0 ${version}
to update it explicitly, or set GC_INSTALL_DIR to a different, non-live
directory for a side-by-side install.
MSG
  exit 1
else
  install_dir="$HOME/.local/bin"
fi

dolt_version="${GC_DOLT_VERSION:-}"
dolt_sha="${GC_DOLT_SHA256:-}"
bd_version="${GC_BD_VERSION:-}"
bd_sha="${GC_BD_SHA256:-}"

case "$(uname -s)" in Darwin) os=darwin ;; Linux) os=linux ;; *) echo "Unsupported OS: $(uname -s)" >&2; exit 1 ;; esac
case "$(uname -m)" in x86_64|amd64) arch=amd64 ;; aarch64|arm64) arch=arm64 ;; *) echo "Unsupported architecture: $(uname -m)" >&2; exit 1 ;; esac

for command in curl tar install cp mv; do command -v "$command" >/dev/null || { echo "Required command not found: $command" >&2; exit 1; }; done
sha256() { if command -v sha256sum >/dev/null; then sha256sum "$1" | awk '{print $1}'; else shasum -a 256 "$1" | awk '{print $1}'; fi; }
require_sha() { [[ "$1" =~ ^[0-9a-f]{64}$ ]] || { echo "Missing or malformed checksum for $2" >&2; exit 1; }; }
download() { curl --fail --location --retry 3 --retry-all-errors --silent --show-error "$1" --output "$2"; }

# Authenticated fetch for GC_INSTALL_REPO (gcf-aetk): it defaults to a
# PRIVATE repo, so a plain unauthenticated request 404s -- for the VERSIONS
# manifest AND gc's own release assets, not just one of them. Tried in
# order: the gh CLI, if it is on PATH and already authenticated; a
# GH_TOKEN/GITHUB_TOKEN env var via the GitHub API directly; a plain
# unauthenticated request as a last-resort fallback (only useful if $repo
# happens to be public); then fail closed. The token is NEVER passed as a
# curl/gh argument (visible to any other user via `ps` on a shared host):
# it goes to curl over stdin as an -H @- header, one line, nothing else on
# that process's argv.
gh_ok=false
if command -v gh >/dev/null 2>&1 && gh auth status >/dev/null 2>&1; then gh_ok=true; fi
gh_token="${GH_TOKEN:-${GITHUB_TOKEN:-}}"
auth_get() {
  # auth_get API_PATH ACCEPT DEST -- authenticated GET against
  # api.github.com/repos/${repo}/API_PATH, response saved to DEST.
  local api_path="$1" accept="$2" dest="$3"
  if "$gh_ok"; then
    gh api -H "Accept: ${accept}" "repos/${repo}/${api_path}" >"$dest" 2>/dev/null
    return $?
  fi
  if [[ -n "$gh_token" ]]; then
    printf 'Authorization: Bearer %s\n' "$gh_token" \
      | curl --fail --location --retry 3 --retry-all-errors --silent --show-error \
          -H "Accept: ${accept}" -H @- \
          "https://api.github.com/repos/${repo}/${api_path}" --output "$dest"
    return $?
  fi
  return 2
}
auth_unavailable_msg="install/authenticate the gh CLI (gh auth login), or export GH_TOKEN/GITHUB_TOKEN"

# Release manifest (VERSIONS): fills in whichever of GC_BD_VERSION/
# GC_BD_SHA256/GC_DOLT_VERSION/GC_DOLT_SHA256 were not supplied explicitly.
# Explicit env values always win — this only fills gaps, never overrides.
# Only fetched when something is actually missing, so a caller who pins
# every value by hand never touches the network for it.
need_bd_manifest=false
if [[ -z "$bd_version" || -z "$bd_sha" ]]; then need_bd_manifest=true; fi
need_dolt_manifest=false
if [[ "$os" == linux && ( -z "$dolt_version" || -z "$dolt_sha" ) ]]; then need_dolt_manifest=true; fi

if "$need_bd_manifest" || "$need_dolt_manifest"; then
  manifest_tmp="$(mktemp)"
  if [[ -n "${GC_MANIFEST_URL:-}" ]]; then
    # Explicit override (tests, or a caller who knows better): used exactly
    # as given, unauthenticated, no auth_get involved.
    manifest_url="$GC_MANIFEST_URL"
    curl --fail --location --retry 3 --retry-all-errors --silent --show-error "$manifest_url" --output "$manifest_tmp" || {
      rm -f "$manifest_tmp"
      echo "Failed to fetch release manifest for v${version} from ${manifest_url}; supply GC_BD_VERSION/GC_BD_SHA256 (and on Linux GC_DOLT_VERSION/GC_DOLT_SHA256) explicitly instead." >&2
      exit 1
    }
  else
    manifest_url="https://raw.githubusercontent.com/${repo}/v${version}/VERSIONS"
    if ! auth_get "contents/VERSIONS?ref=v${version}" "application/vnd.github.raw" "$manifest_tmp"; then
      if ! curl --fail --location --retry 3 --retry-all-errors --silent --show-error "$manifest_url" --output "$manifest_tmp"; then
        rm -f "$manifest_tmp"
        echo "Failed to fetch release manifest for v${version}: no authenticated access to ${repo} (${auth_unavailable_msg}) and the unauthenticated fallback (${manifest_url}) also failed. Supply GC_BD_VERSION/GC_BD_SHA256 (and on Linux GC_DOLT_VERSION/GC_DOLT_SHA256) explicitly instead." >&2
        exit 1
      fi
    fi
  fi
  manifest_raw="$(cat "$manifest_tmp")"
  rm -f "$manifest_tmp"
  # Parse as inert data, never source/eval: this is remote content. Only a
  # strict KEY=VALUE shape is accepted; anything else (a hostile line with
  # shell metacharacters, command substitution, spaces) is silently dropped
  # by the loop below, which leaves the corresponding required value empty
  # and lets the existing required-var/require_sha checks fail closed on it.
  declare -A manifest
  while IFS= read -r manifest_line; do
    if [[ "$manifest_line" =~ ^([A-Z0-9_]+)=([A-Za-z0-9._-]+)$ ]]; then
      manifest["${BASH_REMATCH[1]}"]="${BASH_REMATCH[2]}"
    fi
  done <<<"$manifest_raw"

  manifest_gc_version="${manifest[GC_VERSION]:-}"
  [[ "$manifest_gc_version" == "$version" ]] || {
    echo "Release manifest GC_VERSION (${manifest_gc_version:-<missing>}) does not match requested version ${version} (from ${manifest_url}); refusing to use it." >&2
    exit 1
  }

  manifest_os="$(printf '%s' "$os" | tr '[:lower:]' '[:upper:]')"
  manifest_arch="$(printf '%s' "$arch" | tr '[:lower:]' '[:upper:]')"

  if "$need_bd_manifest"; then
    [[ -n "$bd_version" ]] || bd_version="${manifest[BD_VERSION]:-}"
    [[ -n "$bd_sha" ]] || bd_sha="${manifest["BD_SHA256_${manifest_os}_${manifest_arch}"]:-}"
  fi
  if "$need_dolt_manifest"; then
    [[ -n "$dolt_version" ]] || dolt_version="${manifest[DOLT_VERSION]:-}"
    [[ -n "$dolt_sha" ]] || dolt_sha="${manifest["DOLT_SHA256_${manifest_os}_${manifest_arch}"]:-}"
  fi
fi

# gastownhall/beads release tags carry a "v" prefix in their download URL
# (unlike Dolt's, which this script prepends itself below); normalize here
# so both an explicit GC_BD_VERSION and a manifest-sourced BD_VERSION (which
# is recorded without the prefix, e.g. "1.3.0-rc.2") resolve the same asset.
if [[ -n "$bd_version" && "$bd_version" != v* ]]; then bd_version="v${bd_version}"; fi

[[ -n "$bd_version" ]] || { echo "GC_BD_VERSION is required (not set, and not resolvable from the release manifest)" >&2; exit 2; }
require_sha "$bd_sha" "GC_BD_SHA256"

# gcf-ocze ruling (option B, 2026-09-08): on darwin the Dolt pin is satisfied
# by Homebrew, not by this installer. Refuse loudly and name the owner + the
# version actually found, rather than silently skipping (the exact failure
# QA caught in the install-dir default) or silently installing a second dolt
# that shadows Homebrew's on PATH.
install_dolt=true
if [[ "$os" == darwin ]]; then
  install_dolt=false
  found="$(command -v dolt 2>/dev/null || true)"
  if [[ -n "$found" ]]; then
    found_version="$(dolt version 2>/dev/null | grep -oiE 'dolt version [0-9.]+' | grep -oE '[0-9.]+' | head -n1 || true)"
    echo "Not installing Dolt: darwin's Dolt is Homebrew-owned (found ${found} reporting ${found_version:-unknown}). Use 'brew upgrade dolt' to change it." >&2
  else
    echo "Not installing Dolt: darwin's Dolt is Homebrew-owned, and none was found on PATH. Install it with Homebrew ('brew install dolt'), not this installer." >&2
  fi
elif [[ -z "$dolt_version" ]]; then
  echo "GC_DOLT_VERSION is required (not set, and not resolvable from the release manifest)" >&2
  exit 2
else
  require_sha "$dolt_sha" "GC_DOLT_SHA256"
fi

binaries=(gc bd)
"$install_dolt" && binaries+=(dolt)

asset="gascity_${version}_${os}_${arch}.tar.gz"
checksums="gascity_${version}_checksums.txt"
base="https://github.com/${repo}/releases/download/v${version}"
tmp="$(mktemp -d)"
stage="$tmp/stage"
committed=false
rollback() {
  if ! "$committed" && [[ -d "$tmp/backups" ]]; then
    for binary in "${binaries[@]}"; do
      if [[ -e "$tmp/backups/$binary" ]]; then
        mv -f "$tmp/backups/$binary" "$install_dir/$binary" || true
      elif [[ -e "$tmp/created/$binary" ]]; then
        rm -f "$install_dir/$binary" || true
      fi
    done
  fi
  rm -rf "$tmp"
}
trap rollback EXIT
mkdir -p "$stage" "$tmp/gc" "$tmp/bd"

# gc's own checksums.txt + platform archive live in ${repo}, which is
# PRIVATE by default (gcf-aetk) -- same auth chain as the manifest: gh CLI,
# then a token via the GitHub API (release metadata to resolve each asset's
# numeric id, then a per-id download), then a plain unauthenticated request
# as a last resort, then fail closed.
fetch_gascity_assets() {
  if "$gh_ok"; then
    gh release download "v${version}" -R "$repo" -p "$checksums" -p "$asset" -D "$tmp" >/dev/null 2>&1 && return 0
    return 1
  fi
  if [[ -n "$gh_token" ]]; then
    local release_json="$tmp/.release.json" name id
    auth_get "releases/tags/v${version}" "application/vnd.github+json" "$release_json" || return 1
    for name in "$checksums" "$asset"; do
      # GitHub's API returns pretty-printed JSON, one field per line, "id"
      # a few lines before its asset's "name" (never the reverse) -- track
      # the last "id" seen and emit it the moment the matching "name" line
      # is hit. index() is a plain substring search (not a regex), so an
      # asset name with '.'/'-' in it needs no escaping.
      id="$(awk -v want="\"name\": \"${name}\"" '
        /"id":/ { n=$0; sub(/[^0-9]*"id":[[:space:]]*/, "", n); sub(/[^0-9].*/, "", n); last=n }
        index($0, want) { print last; exit }
      ' "$release_json")"
      [[ -n "$id" ]] || { rm -f "$release_json"; return 1; }
      auth_get "releases/assets/${id}" "application/octet-stream" "$tmp/${name}" || { rm -f "$release_json"; return 1; }
    done
    rm -f "$release_json"
    return 0
  fi
  return 2
}
if ! fetch_gascity_assets; then
  if ! { download "$base/$checksums" "$tmp/$checksums" && download "$base/$asset" "$tmp/$asset"; }; then
    echo "Failed to download gc release assets for v${version} from ${repo}: no authenticated access (${auth_unavailable_msg}) and the unauthenticated fallback also failed." >&2
    exit 1
  fi
fi
gc_sha="$(awk -v asset="$asset" '$2 == asset || $2 == "*" asset { print $1; exit }' "$tmp/$checksums")"
require_sha "$gc_sha" "$asset"
[[ "$(sha256 "$tmp/$asset")" == "$gc_sha" ]] || { echo "Checksum mismatch for $asset" >&2; exit 1; }
tar -xzf "$tmp/$asset" -C "$tmp/gc"
gc_source="$(find "$tmp/gc" -type f -name gc -perm -u+x -print -quit)"
[[ -n "$gc_source" ]] || { echo "Release archive does not contain executable gc" >&2; exit 1; }
install -m 0755 "$gc_source" "$stage/gc"

if "$install_dolt"; then
  mkdir -p "$tmp/dolt"
  dolt_tuple="${os}-${arch}"
  dolt_asset="dolt-${dolt_tuple}.tar.gz"
  download "https://github.com/dolthub/dolt/releases/download/v${dolt_version}/${dolt_asset}" "$tmp/$dolt_asset"
  [[ "$(sha256 "$tmp/$dolt_asset")" == "$dolt_sha" ]] || { echo "Checksum mismatch for $dolt_asset" >&2; exit 1; }
  tar -xzf "$tmp/$dolt_asset" -C "$tmp/dolt"
  dolt_source="$(find "$tmp/dolt" -type f -name dolt -perm -u+x -print -quit)"
  [[ -n "$dolt_source" ]] || { echo "Dolt archive does not contain executable dolt" >&2; exit 1; }
  install -m 0755 "$dolt_source" "$stage/dolt"
fi

bd_tuple="${os}_${arch}"
bd_asset="beads_${bd_version#v}_${bd_tuple}.tar.gz"
download "https://github.com/gastownhall/beads/releases/download/${bd_version}/${bd_asset}" "$tmp/$bd_asset"
[[ "$(sha256 "$tmp/$bd_asset")" == "$bd_sha" ]] || { echo "Checksum mismatch for $bd_asset" >&2; exit 1; }
tar -xzf "$tmp/$bd_asset" -C "$tmp/bd"
bd_source="$(find "$tmp/bd" -type f -name bd -perm -u+x -print -quit)"
[[ -n "$bd_source" ]] || { echo "bd archive does not contain executable bd" >&2; exit 1; }
install -m 0755 "$bd_source" "$stage/bd"

# gcf-vxqx (2026-09-08): the incident swapped in a binary that could not run
# and still reported success. Prove every staged binary actually executes
# BEFORE it ever touches install_dir, not after.
#
# pm correction 2026-09-08 (gp-wisp-4rblst): the first version of this check
# only asked for non-empty output. The preserved incident artifact
# (`echo gc`) PASSES that: it exits 0 and prints "gc", non-empty. A
# digit-dot-digit shape check would also miss it (and would wrongly accept a
# correct-looking binary of the WRONG version -- a stale archive, an
# interrupted download). Require the output to contain the exact version we
# just asked to install, not merely look plausible.
smoke_test() {
  local bin="$1" name="$2" want="$3" out
  if out="$("$bin" version 2>&1)" && [[ -n "$out" && "$out" == *"$want"* ]]; then return 0; fi
  if out="$("$bin" --version 2>&1)" && [[ -n "$out" && "$out" == *"$want"* ]]; then return 0; fi
  echo "Smoke test failed for $name ($bin): expected output containing '$want', got: ${out:-<empty>}" >&2
  return 1
}
smoke_test "$stage/gc" gc "$version" || { echo "Refusing to install: gc failed its smoke test after download and extraction." >&2; exit 1; }
"$install_dolt" && { smoke_test "$stage/dolt" dolt "$dolt_version" || { echo "Refusing to install: dolt failed its smoke test after download and extraction." >&2; exit 1; }; }
smoke_test "$stage/bd" bd "${bd_version#v}" || { echo "Refusing to install: bd failed its smoke test after download and extraction." >&2; exit 1; }

mkdir -p "$install_dir"
mkdir -p "$tmp/backups" "$tmp/created"
for binary in "${binaries[@]}"; do
  target="$install_dir/$binary"
  temp_target="$install_dir/.${binary}.tmp.$$"
  if [[ -e "$target" ]]; then
    cp -p "$target" "$tmp/backups/$binary"
  else
    : > "$tmp/created/$binary"
  fi
  install -m 0755 "$stage/$binary" "$temp_target"
  mv -f "$temp_target" "$target"
done
committed=true

# The rollback trap only restores backups for a failed attempt; once
# committed those backups are about to be discarded with $tmp. Keep one
# labeled copy of each replaced binary so a regression found after a
# successful install has something to revert to.
#
# gcf-vxqx (2026-09-08): this used to `rm -f` every existing .prev-* before
# writing the new one. This host's only clean recovery copy survived purely
# because there was no second incident run. Write and verify the new backup
# FIRST; only then remove older generations.
version_label() {
  local backup="$1" line=""
  line="$("$backup" version 2>/dev/null | head -n1 || true)"
  line="$(printf '%s' "$line" | grep -oE '[0-9]+\.[0-9]+\.[0-9]+[0-9A-Za-z.+-]*' | head -n1 || true)"
  if [[ -z "$line" ]]; then
    line="unknown-$(date -u +%Y%m%dT%H%M%SZ)"
  fi
  printf '%s' "$line"
}
if [[ -d "$tmp/backups" ]]; then
  for binary in "${binaries[@]}"; do
    backup="$tmp/backups/$binary"
    [[ -e "$backup" ]] || continue
    label="$(version_label "$backup")"
    new_prev="$install_dir/${binary}.prev-${label}"
    # Guarded inside the `if`: a failure here must not abort the whole
    # script under `set -e` -- this step is best-effort, and its failure
    # path is "leave the old backup alone", not "undo a successful install".
    if cp -p "$backup" "$new_prev" 2>/dev/null && cmp -s "$backup" "$new_prev"; then
      find "$install_dir" -maxdepth 1 -name "${binary}.prev-*" ! -name "$(basename "$new_prev")" -exec rm -f {} + 2>/dev/null || true
    else
      echo "Warning: failed to write $new_prev intact; leaving any existing ${binary}.prev-* backups in place." >&2
      rm -f "$new_prev" 2>/dev/null || true
    fi
  done
fi

if "$install_dolt"; then
  echo "Installed gc v${version}, Dolt ${dolt_version}, and bd ${bd_version} to ${install_dir}"
else
  echo "Installed gc v${version} and bd ${bd_version} to ${install_dir} (Dolt left to Homebrew on darwin)"
fi
case ":$PATH:" in *":$install_dir:"*) ;; *) echo "Add ${install_dir} to PATH to use gc." >&2 ;; esac
