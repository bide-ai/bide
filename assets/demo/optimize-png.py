#!/usr/bin/env python3
"""optimize-png.py IN OUT: write IN as a palette PNG (at most 256 colors), optimized.

A terminal screenshot has a few dozen colors plus anti-aliasing, so a 256-color palette is
visually identical and several times smaller. The recording scripts (assets/demo/*.sh) use it.
"""
import sys
from PIL import Image

src, dst = sys.argv[1], sys.argv[2]
im = Image.open(src).convert("RGB")
im.quantize(colors=256, method=Image.Quantize.MEDIANCUT, dither=Image.Dither.NONE).save(dst, optimize=True)
