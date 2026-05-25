# Skill bundle signing (P2)

> Status: shipped in P2 (2026-05-25). Optional — bundles without a signature continue to install unless `POLAR_AGENT_REQUIRE_SIGNED=true`.

Bundles can be cryptographically signed with an Ed25519 key. The agent verifies the signature against the bundle's SHA256 before installing.

## Trust model

```
Publisher keypair (Ed25519)
   │
   ├── private key  ← lives offline at the publisher
   │
   └── public key   ← copied into manifest_json on POST /api/skill-catalog
                       (the catalog row is the agent's trust root)
```

What the signature attests: "the publisher controlling this private key vouches that the .skill ZIP with this exact SHA256 is the one they intended to ship."

The signature is over the **raw 32 bytes** of `SHA256(bundle.skill)`. SHA256 already covers the manifest + scripts + everything inside the ZIP, so signing the SHA gives bundle-level integrity without the agent having to re-hash internals.

## Wire shape

The catalog's `manifest_json` field carries two new keys:

```json
{
  "publisher_pubkey": "<base64 raw 32 bytes>",
  "signature":        "<base64 raw 64 bytes>",
  "...other manifest fields...": "..."
}
```

Both fields are optional. If only one is present, agent rejects with `signature_invalid` (defensive: don't allow half-configured signing).

## Agent behavior

| `manifest_preview` has pubkey+sig | sig verifies | `POLAR_AGENT_REQUIRE_SIGNED` | Outcome |
|---|---|---|---|
| no | n/a | unset / false | install (unsigned legacy path) |
| no | n/a | **true** | reject: `signature_missing` |
| yes | yes | any | install + `signed_by=<pubkey>` in result |
| yes | no | any | reject: `signature_invalid` (reason in error) |

Verification runs **after** SHA256 download check + **before** manifest read. A tampered bundle with a stale signature trips the SHA check first; a tampered SHA in the catalog row trips the signature check.

`skill.install.result` reports `signed_by` (base64 pubkey) when verification ran. UI can surface "Signed by Publisher X" if that's a useful trust badge.

## Publisher workflow

### Generate a publisher key (once per publisher)

```sh
scripts/skill-sign.sh genkey ./com.example.echo.key
```

Stdout shows the base64 public key. Save the private key file somewhere safe (it's the publisher's identity); paste the public key into manifest_json when registering catalog rows.

### Sign each bundle release

```sh
# Build the bundle as usual
cd scripts/example-skill
zip -r ../../com.example.echo_0.1.0.skill . -x "*.DS_Store"

# Sign it
scripts/skill-sign.sh sign ./com.example.echo.key ./com.example.echo_0.1.0.skill
```

Output is a ready-to-paste JSON snippet:

```json
{
  "publisher_pubkey": "...",
  "signature":        "...",
  "_bundle_sha256":   "..."
}
```

Use it as `manifest_json` when POSTing to `/api/skill-catalog`. Make sure the catalog row's `sha256` field matches `_bundle_sha256` (the agent will refuse otherwise).

## Operator workflow

### Enforce signed-only installs on an agent

```sh
# In ~/polar-agent.env (or launchd plist)
POLAR_AGENT_REQUIRE_SIGNED=true
```

Restart polar-agent. All future installs require a verified signature; unsigned bundles trip `signature_missing` and surface in the install result.

### Verify an existing install was signed

```sh
psql -d polar_hosts -c "
  SELECT kind, publisher, install_id, source
    FROM host_skills
   WHERE host_id = 'h_...' AND source = 'bundle';
"
```

`skill.install.result` records `signed_by` for verified installs; the next iteration of the marketplace UI (P2-ui follow-up) will show a 🔒 badge per row.

## Roadmap (not in P2)

- **Publisher key revocation** — operator-side blocklist of pubkeys. Today, rotating the publisher key requires re-signing + re-registering all catalog rows.
- **Sig-on-manifest separately from sig-on-bundle** — currently we only sign the bundle's SHA. Manifest-only signing would let publishers re-sign without rebuilding the ZIP. Skipped because the bundle-SHA approach is simpler and the wire is the same shape.
- **Key transparency log** — out of scope; trust the catalog admin who pasted the pubkey.

## Files

- `cmd/polar-agent/skills/signing.go` — `VerifySkillSignature` + `RequireSigned`
- `cmd/polar-agent/skills/installer.go` — gate runs between SHA check and manifest read
- `scripts/skill-sign.sh` — keygen + sign CLI (openssl-only, no extra deps)
