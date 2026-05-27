#!/usr/bin/env bash
# skill-pack.sh — build a .skill bundle from a source directory.
#
# A .skill file is just a ZIP whose top-level directory matches the
# bundle name (publisher_kind), containing:
#   manifest.yaml     (publisher / kind / version / entrypoint / runtime)
#   scripts/          (entrypoint + any helpers)
#   references/       (markdown docs the LLM or operator reads — optional)
#   requirements.txt  (when runtime.venv: true)
#
# This script does the boring parts: parse publisher/kind/version out of
# manifest.yaml, build the right top-level dir name, exclude junk
# (.DS_Store, __pycache__, .venv, .git), zip, sha256.
#
# It does NOT sign — sign with scripts/skill-sign.sh after building.
#
# Usage:
#   scripts/skill-pack.sh <source-dir> [output-dir]
#
# Examples:
#   # Build the reference example into dist/skills/
#   scripts/skill-pack.sh scripts/example-skill dist/skills
#
#   # Default output dir is ./dist
#   scripts/skill-pack.sh scripts/wg-mac-install
#
# Output:
#   <output-dir>/<publisher>_<kind>_<version>.skill
#   <output-dir>/<publisher>_<kind>_<version>.skill.sha256

set -euo pipefail

usage() {
    cat >&2 <<EOF
usage: $0 <source-dir> [output-dir]

  source-dir: bundle source. Must contain a manifest.yaml at its root
              with at least publisher, kind, and version fields.
  output-dir: where to write the .skill and .sha256 (default: ./dist)
EOF
    exit 2
}

src=${1:-}
out=${2:-./dist}
[[ -z "$src" ]] && usage
if [[ ! -d "$src" ]]; then
    echo "skill-pack: source dir not found: $src" >&2
    exit 1
fi

manifest="$src/manifest.yaml"
if [[ ! -f "$manifest" ]]; then
    echo "skill-pack: missing $manifest" >&2
    exit 1
fi

# Pull the three required fields out of manifest.yaml. We avoid
# yq/python deps — grep is enough for a flat top-level YAML block.
# `awk '/^<key>:/{ ... ; exit}'` stops at the first match so a deeper
# nested key with the same name can't shadow the top-level one.
read_yaml_top() {
    local key=$1
    awk -v k="$key" '
        $0 ~ "^" k ":" {
            sub("^" k "[[:space:]]*:[[:space:]]*", "")
            gsub(/^["'"'"']|["'"'"']$/, "")
            print
            exit
        }' "$manifest"
}

publisher=$(read_yaml_top publisher)
kind=$(read_yaml_top kind)
version=$(read_yaml_top version)

[[ -z "$publisher" ]] && { echo "skill-pack: manifest.yaml missing publisher" >&2; exit 1; }
[[ -z "$kind"      ]] && { echo "skill-pack: manifest.yaml missing kind" >&2; exit 1; }
[[ -z "$version"   ]] && { echo "skill-pack: manifest.yaml missing version" >&2; exit 1; }

# Top-level dir inside the ZIP — matches the on-disk install path the
# agent uses and the publisher/kind/version triple in the manifest.
# Pattern: <publisher>_<kind>  (Anthropic-style without version in dir
# name; version lives in the filename + manifest).
bundle_name="${publisher}_${kind}"
filename="${publisher}_${kind}_${version}.skill"
mkdir -p "$out"
out_path="$out/$filename"
sha_path="${out_path}.sha256"

# Stage into a temp dir so the ZIP's top-level entry name is exactly
# <bundle_name>/ regardless of what the source dir is called locally.
staging=$(mktemp -d)
trap 'rm -rf "$staging"' EXIT

cp -R "$src/." "$staging/$bundle_name/"

# Excludes — these are files no one needs in a published bundle, and
# leaking a local .venv would balloon the ZIP by hundreds of MB.
find "$staging/$bundle_name" \
    -name ".DS_Store"     -delete -o \
    -name "__pycache__"   -type d -exec rm -rf {} + -o \
    -name ".git"          -type d -exec rm -rf {} + -o \
    -name ".venv"         -type d -exec rm -rf {} + 2>/dev/null || true

# `zip -X` strips extra file attrs so the same input produces the
# same bytes across re-builds (useful for sha256 stability).
( cd "$staging" && zip -qrX "$out_path" "$bundle_name" )

# sha256 — use shasum on macOS, sha256sum on Linux. Output format
# matches what the agent's signature verifier and dock's catalog
# upload both consume.
if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$out_path" | awk '{print $1}' > "$sha_path"
elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$out_path" | awk '{print $1}' > "$sha_path"
else
    echo "skill-pack: neither sha256sum nor shasum on PATH; .skill produced but no checksum written" >&2
fi

size=$(wc -c < "$out_path" | tr -d ' ')
sha=$(cat "$sha_path" 2>/dev/null || echo "(checksum missing)")

cat <<EOF
[skill-pack] built $filename
  path:    $out_path
  size:    $size bytes
  sha256:  $sha
  contents:
$(unzip -l "$out_path" | sed 's/^/    /')
EOF
