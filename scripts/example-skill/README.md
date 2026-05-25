# example-skill

Minimal reference bundle showing the `.skill` format. Read line, write line. ~10 LOC of Python.

## Build

```sh
cd scripts/example-skill
zip -r ../../com.networkextension.example_echo_0.1.0.skill . -x "*.DS_Store"
shasum -a 256 ../../com.networkextension.example_echo_0.1.0.skill
```

## Verify locally

```sh
# Bundle a manifest.yaml + scripts/run.py is enough — agent parses + validates
go run ../../cmd/polar-agent-test verify-bundle ../../com.networkextension.example_echo_0.1.0.skill
```

## Use as a template

1. Copy this directory
2. Rename, edit `manifest.yaml` (publisher / kind / version / capabilities.tools)
3. Replace `scripts/run.py` with your actual entrypoint
4. Add `requirements.txt` and set `runtime.venv: true` if you need pip dependencies
5. Re-zip; new SHA256
6. Register with catalog (P0b — REST endpoint)

See [`../../doc/bundle-format.md`](../../doc/bundle-format.md) for the full schema.
