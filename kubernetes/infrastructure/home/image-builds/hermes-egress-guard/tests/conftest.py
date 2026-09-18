"""Test bootstrap for the guard package.

The package lives in ``../src`` and is stdlib-only, so tests add it to
``sys.path`` instead of installing it (mirrors the Dockerfile, which copies
``src/`` to ``/opt/egress-guard`` and sets ``PYTHONPATH``).
"""

from __future__ import annotations

import sys
from pathlib import Path

SRC = Path(__file__).resolve().parents[1] / "src"
if str(SRC) not in sys.path:
    sys.path.insert(0, str(SRC))
