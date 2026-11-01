"""python -m dagster_hangar bake … | version"""

import sys

from dagster_hangar import __version__
from dagster_hangar.settings import SettingsError, read

USAGE = """usage: python -m dagster_hangar bake --name NAME [IMAGE …] [--help]
       python -m dagster_hangar version

bake     the image a run's machine is born from (python -m dagster_hangar bake --help)
version  this package's version
"""


def main(argv: list[str]) -> int:
    if not argv or argv[0] in ("-h", "--help", "help"):
        print(USAGE, end="")
        return 0 if argv else 2
    if argv[0] == "version":
        print(__version__)
        return 0
    if argv[0] == "bake":
        from dagster_hangar import bake

        try:
            settings = read({})
        except SettingsError as e:
            print(f"bake: {e}", file=sys.stderr)
            return 1
        return bake.main(argv[1:], settings)
    print(USAGE, end="", file=sys.stderr)
    return 2


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
