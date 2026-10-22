# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
"""Runs one of Mesa's code generators, as Mesa's build does.

Usage: run.py [--capture OUT] SCRIPT [ARGS...]

The generators import modules beside them and Mako, MarkupSafe, and PyYAML,
which this binary's dependencies provide. With --capture, the generator's
standard output goes to OUT, as meson's `--internal exe --capture` has it.
"""

import os
import runpy
import sys


def main():
    args = sys.argv[1:]
    capture = None
    if args[:1] == ["--capture"]:
        capture = args[1]
        args = args[2:]
    if not args:
        sys.exit("usage: run.py [--capture OUT] SCRIPT [ARGS...]")
    script = args[0]
    sys.argv = [script] + args[1:]
    sys.path.insert(0, os.path.dirname(os.path.abspath(script)))
    if capture is None:
        runpy.run_path(script, run_name="__main__")
        return
    with open(capture, "w") as out:
        stdout = sys.stdout
        sys.stdout = out
        try:
            runpy.run_path(script, run_name="__main__")
        finally:
            sys.stdout = stdout


if __name__ == "__main__":
    main()
