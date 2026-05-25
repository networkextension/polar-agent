#!/usr/bin/env bash
# skill-sign.sh — sign a .skill ZIP with an Ed25519 publisher key.
#
# Emits a JSON snippet ready to paste into the manifest_json field
# of POST /api/skill-catalog. The agent's signature verifier consumes
# the same shape.
#
# Workflow:
#   1. Generate a publisher key pair (once per publisher):
#        scripts/skill-sign.sh genkey ./com.example.echo.key
#      → writes private key to ./com.example.echo.key (chmod 600).
#      → prints the matching base64 public key on stdout.
#   2. Sign a .skill bundle:
#        scripts/skill-sign.sh sign ./com.example.echo.key ./echo-0.1.0.skill
#      → prints the manifest_json snippet on stdout.
#
# No external deps beyond openssl (with ed25519 support — openssl ≥ 1.1.1).

set -euo pipefail

usage() {
    cat <<EOF
usage:
  $0 genkey  <out-private-key.pem>
  $0 sign    <private-key.pem> <bundle.skill>
EOF
    exit 1
}

cmd=${1:-}
case "$cmd" in
    genkey)
        out=${2:-}
        [[ -z "$out" ]] && usage
        openssl genpkey -algorithm ED25519 -out "$out"
        chmod 600 "$out"
        # Extract the raw 32-byte public key, then base64-encode.
        pub_b64=$(openssl pkey -in "$out" -pubout -outform DER \
            | tail -c 32 | base64 | tr -d '\n')
        echo "publisher_pubkey: $pub_b64"
        echo
        echo "Save the private key ($out) somewhere safe — it's the publisher's"
        echo "identity. Paste publisher_pubkey into manifest_json when registering"
        echo "catalog rows. Anyone with the matching sig can install signed bundles"
        echo "into agents with POLAR_AGENT_REQUIRE_SIGNED=true."
        ;;

    sign)
        key=${2:-}
        bundle=${3:-}
        [[ -z "$key" || -z "$bundle" ]] && usage
        [[ ! -f "$key" ]] && { echo "key not found: $key" >&2; exit 2; }
        [[ ! -f "$bundle" ]] && { echo "bundle not found: $bundle" >&2; exit 2; }

        # SHA256(bundle) — raw 32 bytes.
        sha_bin=$(mktemp)
        trap 'rm -f "$sha_bin"' EXIT
        openssl dgst -sha256 -binary -out "$sha_bin" "$bundle"
        sha_hex=$(xxd -p -c 64 < "$sha_bin")

        # Sign the raw SHA bytes with ed25519. openssl ed25519 only
        # accepts -rawin (signs the literal input bytes).
        sig_bin=$(mktemp)
        trap 'rm -f "$sha_bin" "$sig_bin"' EXIT
        openssl pkeyutl -sign -inkey "$key" -rawin -in "$sha_bin" -out "$sig_bin"
        sig_b64=$(base64 < "$sig_bin" | tr -d '\n')

        pub_b64=$(openssl pkey -in "$key" -pubout -outform DER \
            | tail -c 32 | base64 | tr -d '\n')

        cat <<EOF
{
  "publisher_pubkey": "$pub_b64",
  "signature":        "$sig_b64",
  "_bundle_sha256":   "$sha_hex"
}
EOF
        echo >&2
        echo "Bundle SHA256: $sha_hex" >&2
        echo "Use this JSON as the manifest_json field on POST /api/skill-catalog." >&2
        echo "Make sure the catalog row's 'sha256' field matches _bundle_sha256." >&2
        ;;

    *) usage ;;
esac
