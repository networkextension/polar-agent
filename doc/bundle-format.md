# `.skill` Bundle Format (v0)

> Authoritative spec for a polar-agent skill bundle. Mirror of `bundle-skill-dev.md` §5 — change both together or neither.
> Status: **shipped in P0a (2026-05-25)** — parser lives at `cmd/polar-agent/skills/bundle_manifest.go`. Backward-compatible: bundles without `manifest.yaml` continue to work using the legacy dock-supplied `BundleConfig`.

## 1 · Layout

A `.skill` bundle is a ZIP file. Suggested extension: `.skill`. Suggested filename: `<publisher>_<kind>_<version>.skill`.

```
my-skill.skill/
├── manifest.yaml         ← REQUIRED (P0a: required to use new fields; legacy bundles still work)
├── README.md             ← recommended
├── LICENSE               ← recommended
├── scripts/
│   └── run.py            ← entrypoint (path relative to bundle root)
├── requirements.txt      ← optional; if present + runtime.venv=true → agent runs pip install
└── data/                 ← optional static assets
```

Constraints:

- Total ZIP size ≤ `bundleMaxBytes` (256 MiB) — operator can override via `install.size_max_mb` in manifest, but agent ignores requests above its own hard cap
- Entrypoint MUST live under `scripts/` (no path traversal)
- ZIP must not contain absolute paths or `..` segments (agent rejects on extract — zip-slip protection)

## 2 · `manifest.yaml` Schema

```yaml
# ── required ──────────────────────────────────────────────────
publisher: com.example.echo            # reverse-domain; [a-z0-9.-]+
kind: echo                              # [a-z0-9.-]+
version: 0.1.0                          # opaque to agent; semver convention
entrypoint: scripts/run.py              # relative path under scripts/

# ── recommended ───────────────────────────────────────────────
display_name: Echo
description: |
  Read lines from stdin, write them back to stdout. Reference skill.
license: Apache-2.0
homepage: https://github.com/networkextension/example-skills

# ── runtime ───────────────────────────────────────────────────
runtime:
  kind: python                          # python | shell | binary; default inferred from entrypoint
  python_version: ">=3.10"              # advisory; agent doesn't enforce
  venv: true                            # if true + requirements.txt present → agent does `python -m venv .venv && pip install -r requirements.txt`
  args: []                              # appended to entrypoint argv; operator can override via BundleConfig.Args
  env:                                  # default env; operator overrides via BundleConfig.Env (per-key)
    LOG_LEVEL: info

# ── capabilities (free-form; advertised verbatim) ─────────────
capabilities:
  tools:                                # list of named tools this skill exposes (dock UI uses name as stable handle)
    - name: echo
      description: Identity transform on text

# ── requirements (host gates) ─────────────────────────────────
requires:
  - usb                                 # bundle wants USB access
  # - python: ">=3.10"                  # version constraint syntax: `name: constraint`
  # other recognised names: network, network_admin, macos, linux, root, macos_app_sandbox_exempt
  # Unknown names fail Validate() at parse time — fail closed.

# ── install hints (optional) ──────────────────────────────────
install:
  size_max_mb: 50                       # advisory; agent's hard cap still wins
  timeout_seconds: 120
```

## 3 · Precedence: manifest vs `BundleConfig`

When both a manifest and a dock-sent `BundleConfig` are present, the agent merges them as follows (see `BundleManifest.MergeOntoConfig`):

| Field | Winner |
|---|---|
| `entrypoint` | manifest always wins (canonical) |
| `args` | `BundleConfig.Args` if non-empty; else manifest |
| `env` | manifest defaults + `BundleConfig.Env` overrides per key |
| `requires` | manifest only — `BundleConfig` has no equivalent today |
| `capabilities` | manifest only |
| `runtime.kind` | manifest only |

## 4 · SHA256

The bundle catalog (P0b) stores `sha256(.skill ZIP)` as content fingerprint.

- Agent verifies SHA after download, **before** extraction (`bundle.go`)
- Mismatch → install fails with `skill.install.result {status: "sha_mismatch"}` (P1a)
- SHA covers the entire ZIP — internal-to-ZIP integrity is provided by the ZIP CRCs (double protection)

## 5 · Identity

```
<publisher>/<kind>@<version>
```

Examples:
- `com.networkextension.kdp/kdp@0.3.0`
- `com.example.echo/echo@0.1.0`
- `platform/shell@1.0.0` (the built-in shell skill — `platform/` reserved for compiled-in skills with no manifest)

## 6 · Failure modes (parse / validate)

| Cause | Error class |
|---|---|
| File missing | `nil, nil` (manifest is optional, not an error) |
| YAML syntax | `parse manifest yaml: ...` |
| Missing `publisher` / `kind` / `version` / `entrypoint` | `validate manifest: <field> is required` |
| Bad `publisher` (not reverse-domain) | `validate manifest: expected reverse-domain form` |
| Absolute or `..` `entrypoint` | `validate manifest: entrypoint ... must be relative` |
| Unknown `runtime.kind` | `validate manifest: runtime.kind ... must be python | shell | binary` |
| Duplicate tool name | `validate manifest: capabilities.tools: duplicate name` |
| Unknown `requires` entry | `validate manifest: requires: unknown requirement ...` |

## 7 · Building a bundle

```sh
cd example-skill
zip -r ../com.networkextension.example_echo_0.1.0.skill . -x "*.DS_Store"
shasum -a 256 ../com.networkextension.example_echo_0.1.0.skill
```

Register with polar-hosts catalog (P0b — coming in next PR):

```sh
curl -X POST https://zen.4950.store/api/skill-catalog \
  -H "Content-Type: application/json" \
  -d '{
    "publisher": "com.networkextension.example",
    "skill_kind": "echo",
    "version": "0.1.0",
    "sha256": "<from shasum above>",
    "download_url": "https://r2.example.com/.../com.networkextension.example_echo_0.1.0.skill",
    "size_bytes": 1024,
    "is_platform": true
  }'
```

Reference bundle: see `scripts/example-skill/`.

## 8 · Roadmap (out of scope for this doc)

- **P0b** — catalog table in polar-hosts
- **P1a** — `skill.install` / `skill.uninstall` WS frames
- **P1b** — `/skills.html` marketplace UI
- **P2** — Ed25519 signing
- **P3** — workspace ownership (platform vs private bundles)

Tracked in [`bundle-skill-dev.md`](./bundle-skill-dev.md) §7.
