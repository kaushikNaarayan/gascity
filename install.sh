#!/usr/bin/env bash
# Install a pinned Gas City release and its runtime data-plane dependencies.
#
# Usage: curl -fsSL https://raw.githubusercontent.com/material-lab-io/gascity/main/install.sh | bash
#        ./install.sh 1.3.7
set -euo pipefail

repo="${GC_INSTALL_REPO:-material-lab-io/gascity}"
version="${1:-${GC_VERSION:-}}"
if [[ -n "${GC_INSTALL_DIR:-}" ]]; then
  install_dir="$GC_INSTALL_DIR"
elif live_gc="$(command -v gc 2>/dev/null)"; then
  # A gc already resolves on PATH: that is the live install this machine
  # actually runs. Target it, or a default-path install silently misses it
  # and reports success having changed nothing.
  install_dir="$(cd "$(dirname "$live_gc")" && pwd)"
else
  install_dir="$HOME/.local/bin"
fi
dolt_version="${GC_DOLT_VERSION:-}"
dolt_sha="${GC_DOLT_SHA256:-}"
bd_version="${GC_BD_VERSION:-}"
bd_sha="${GC_BD_SHA256:-}"

usage() {
  cat <<'EOF'
Usage: install.sh [VERSION]

Installs gc VERSION and a caller-supplied, exact Dolt/bd release tuple. VERSION
may also be supplied through GC_VERSION. Existing binaries are replaced only
after every archive has downloaded, passed SHA-256 verification, and unpacked.

Required environment: GC_DOLT_VERSION, GC_DOLT_SHA256, GC_BD_VERSION,
GC_BD_SHA256. Supply the checksum for the selected OS/architecture asset from
the release manifest. This installer deliberately has no dependency defaults:
the release manager owns the published gc/Dolt/bd compatibility tuple.

Optional environment: GC_INSTALL_DIR, GC_INSTALL_REPO. Without GC_INSTALL_DIR,
the installer targets the directory of whatever gc already resolves on PATH
(so an existing install is updated in place, not silently shadowed); only
when no gc is found on PATH does it fall back to ~/.local/bin.

On success, the previous gc/dolt/bd binaries are kept beside the new ones as
<binary>.prev-<old-version> for a manual revert.
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

case "$(uname -s)" in Darwin) os=darwin ;; Linux) os=linux ;; *) echo "Unsupported OS: $(uname -s)" >&2; exit 1 ;; esac
case "$(uname -m)" in x86_64|amd64) arch=amd64 ;; aarch64|arm64) arch=arm64 ;; *) echo "Unsupported architecture: $(uname -m)" >&2; exit 1 ;; esac

for command in curl tar install cp mv; do command -v "$command" >/dev/null || { echo "Required command not found: $command" >&2; exit 1; }; done
sha256() { if command -v sha256sum >/dev/null; then sha256sum "$1" | awk '{print $1}'; else shasum -a 256 "$1" | awk '{print $1}'; fi; }
require_sha() { [[ "$1" =~ ^[0-9a-f]{64}$ ]] || { echo "Missing or malformed checksum for $2" >&2; exit 1; }; }
[[ -n "$dolt_version" ]] || { echo "GC_DOLT_VERSION is required" >&2; exit 2; }
[[ -n "$bd_version" ]] || { echo "GC_BD_VERSION is required" >&2; exit 2; }
require_sha "$dolt_sha" "GC_DOLT_SHA256"
require_sha "$bd_sha" "GC_BD_SHA256"

asset="gascity_${version}_${os}_${arch}.tar.gz"
checksums="gascity_${version}_checksums.txt"
base="https://github.com/${repo}/releases/download/v${version}"
tmp="$(mktemp -d)"
stage="$tmp/stage"
committed=false
rollback() {
  if ! "$committed" && [[ -d "$tmp/backups" ]]; then
    for binary in gc dolt bd; do
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
mkdir -p "$stage" "$tmp/gc" "$tmp/dolt" "$tmp/bd"

download() { curl --fail --location --retry 3 --retry-all-errors --silent --show-error "$1" --output "$2"; }
download "$base/$checksums" "$tmp/$checksums"
gc_sha="$(awk -v asset="$asset" '$2 == asset || $2 == "*" asset { print $1; exit }' "$tmp/$checksums")"
require_sha "$gc_sha" "$asset"
download "$base/$asset" "$tmp/$asset"
[[ "$(sha256 "$tmp/$asset")" == "$gc_sha" ]] || { echo "Checksum mismatch for $asset" >&2; exit 1; }
tar -xzf "$tmp/$asset" -C "$tmp/gc"
gc_source="$(find "$tmp/gc" -type f -name gc -perm -u+x -print -quit)"
[[ -n "$gc_source" ]] || { echo "Release archive does not contain executable gc" >&2; exit 1; }
install -m 0755 "$gc_source" "$stage/gc"

dolt_tuple="${os}-${arch}"
dolt_asset="dolt-${dolt_tuple}.tar.gz"
download "https://github.com/dolthub/dolt/releases/download/v${dolt_version}/${dolt_asset}" "$tmp/$dolt_asset"
[[ "$(sha256 "$tmp/$dolt_asset")" == "$dolt_sha" ]] || { echo "Checksum mismatch for $dolt_asset" >&2; exit 1; }
tar -xzf "$tmp/$dolt_asset" -C "$tmp/dolt"
dolt_source="$(find "$tmp/dolt" -type f -name dolt -perm -u+x -print -quit)"
[[ -n "$dolt_source" ]] || { echo "Dolt archive does not contain executable dolt" >&2; exit 1; }
install -m 0755 "$dolt_source" "$stage/dolt"

bd_tuple="${os}_${arch}"
bd_asset="beads_${bd_version#v}_${bd_tuple}.tar.gz"
download "https://github.com/gastownhall/beads/releases/download/${bd_version}/${bd_asset}" "$tmp/$bd_asset"
[[ "$(sha256 "$tmp/$bd_asset")" == "$bd_sha" ]] || { echo "Checksum mismatch for $bd_asset" >&2; exit 1; }
tar -xzf "$tmp/$bd_asset" -C "$tmp/bd"
bd_source="$(find "$tmp/bd" -type f -name bd -perm -u+x -print -quit)"
[[ -n "$bd_source" ]] || { echo "bd archive does not contain executable bd" >&2; exit 1; }
install -m 0755 "$bd_source" "$stage/bd"

mkdir -p "$install_dir"
mkdir -p "$tmp/backups" "$tmp/created"
for binary in gc dolt bd; do
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
  for binary in gc dolt bd; do
    backup="$tmp/backups/$binary"
    [[ -e "$backup" ]] || continue
    label="$(version_label "$backup")"
    rm -f "$install_dir/${binary}.prev-"* 2>/dev/null || true
    cp -p "$backup" "$install_dir/${binary}.prev-${label}"
  done
fi

echo "Installed gc v${version}, Dolt ${dolt_version}, and bd ${bd_version} to ${install_dir}"
case ":$PATH:" in *":$install_dir:"*) ;; *) echo "Add ${install_dir} to PATH to use gc." >&2 ;; esac
