#!/usr/bin/env bash
# Install a pinned Gas City release and its runtime data-plane dependencies.
#
# Usage: curl -fsSL https://raw.githubusercontent.com/material-lab-io/gascity/main/install.sh | bash
#        ./install.sh 1.3.7
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

Required environment: GC_BD_VERSION, GC_BD_SHA256, and on Linux also
GC_DOLT_VERSION/GC_DOLT_SHA256. Supply the checksum for the selected OS/
architecture asset from the release manifest. This installer deliberately has
no dependency defaults: the release manager owns the published gc/Dolt/bd
compatibility tuple.

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

Optional environment: GC_INSTALL_REPO.
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
[[ -n "$bd_version" ]] || { echo "GC_BD_VERSION is required" >&2; exit 2; }
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
  echo "GC_DOLT_VERSION is required" >&2
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
smoke_test() {
  local bin="$1" name="$2" out
  if out="$("$bin" version 2>&1)" && [[ -n "$out" ]]; then return 0; fi
  if out="$("$bin" --version 2>&1)" && [[ -n "$out" ]]; then return 0; fi
  echo "Smoke test failed for $name ($bin): it does not run a version command." >&2
  return 1
}
for binary in "${binaries[@]}"; do
  smoke_test "$stage/$binary" "$binary" || { echo "Refusing to install: $binary failed its smoke test after download and extraction." >&2; exit 1; }
done

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
