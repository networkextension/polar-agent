# example-skill

Minimal reference bundle showing the `.skill` format. Read line, write line. ~10 LOC of Python.

## Build

```sh
scripts/skill-pack.sh scripts/example-skill
# → dist/com.networkextension.example_echo_0.1.0.skill
# → dist/com.networkextension.example_echo_0.1.0.skill.sha256
```

`skill-pack.sh` reads `publisher` / `kind` / `version` from
`manifest.yaml`, stages the source into a top-level
`<publisher>_<kind>/` dir, strips junk (`.DS_Store`, `__pycache__`,
`.venv`, `.git`), and writes both the bundle and its checksum. The
same script handles every bundle in the repo — point it at any
directory with a `manifest.yaml` at the root.

## Sign

After building, sign with `scripts/skill-sign.sh`:

```sh
scripts/skill-sign.sh sign ./com.networkextension.example.key \
    dist/com.networkextension.example_echo_0.1.0.skill
```

## Use as a template

1. Copy this directory
2. Rename, edit `manifest.yaml` (publisher / kind / version / capabilities.tools)
3. Replace `scripts/run.py` with your actual entrypoint
4. Add `requirements.txt` and set `runtime.venv: true` if you need pip dependencies
5. Re-build with `scripts/skill-pack.sh <your-dir>`
6. Register with catalog (P0b — REST endpoint)

See [`../../doc/bundle-format.md`](../../doc/bundle-format.md) for the full schema.
