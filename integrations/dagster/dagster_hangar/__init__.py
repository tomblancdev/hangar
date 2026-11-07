"""Dagster's runs, each on a machine of Le Hangar.

    run_launcher:
      module: dagster_hangar
      class: HangarRunLauncher
      config:
        default_size: 2x4

The deployment's settings come from HANGAR_* variables (settings.py); with
no HANGAR_URL every run runs in-process. The bake: python -m dagster_hangar bake.
"""

__version__ = "0.10.0"
__all__ = ["HangarRunLauncher", "__version__"]


def __getattr__(name: str):
    # the launcher imports Dagster; the bake and the version need none of it
    if name == "HangarRunLauncher":
        from dagster_hangar.launcher import HangarRunLauncher

        return HangarRunLauncher
    raise AttributeError(f"module 'dagster_hangar' has no attribute {name!r}")
