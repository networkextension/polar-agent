#!/usr/bin/env python3
"""Echo skill — read stdin lines, write them back to stdout.

Reference entrypoint for the .skill bundle format. Keep this tiny so
copy-pasting the bundle for a new skill is mostly editing manifest.yaml.
"""
import sys


def main() -> int:
    for line in sys.stdin:
        sys.stdout.write(line)
        sys.stdout.flush()
    return 0


if __name__ == "__main__":
    sys.exit(main())
