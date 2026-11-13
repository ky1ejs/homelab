"""Draw the background (paper plus the accent glow behind the Studio) in
floating point, dither it to 8 bits, and lay render.mjs's PNG on top.

The glow spans only a few 8-bit steps, so a plain render shows rings; the
dither hides them. Usage: python3 compose.py [dark|light]
"""
import sys
import numpy as np
from PIL import Image

THEMES = {"dark": ("#121318", "#2A1E3D"), "light": ("#F3F5F8", "#F1E8FB")}
W, H = 6016, 3384


def rgb(h):
    return np.array([int(h[i:i + 2], 16) for i in (1, 3, 5)], dtype=np.float64)


for theme, (paper, soft) in THEMES.items():
    if sys.argv[1:] and theme not in sys.argv[1:]:
        continue
    y, x = np.mgrid[0:H, 0:W].astype(np.float64)
    r = np.hypot(x - W / 2, y - H * 0.40) / (W * 0.36)
    t = np.clip(1 - r, 0, 1) ** 2.2  # soft falloff
    img = rgb(paper) + (rgb(soft) - rgb(paper)) * t[..., None]
    # Ordered (Bayer 8x8) dither: breaks the gradient's 8-bit banding, and its
    # repeating pattern compresses far better in PNG than random noise.
    b = np.array([
        [0, 32, 8, 40, 2, 34, 10, 42], [48, 16, 56, 24, 50, 18, 58, 26],
        [12, 44, 4, 36, 14, 46, 6, 38], [60, 28, 52, 20, 62, 30, 54, 22],
        [3, 35, 11, 43, 1, 33, 9, 41], [51, 19, 59, 27, 49, 17, 57, 25],
        [15, 47, 7, 39, 13, 45, 5, 37], [63, 31, 55, 23, 61, 29, 53, 21],
    ]) / 64.0
    thr = np.tile(b, (H // 8 + 1, W // 8 + 1))[:H, :W, None]
    bg = Image.fromarray(np.clip(np.floor(img + thr), 0, 255).astype(np.uint8), "RGB")
    fg = Image.open(f"fg-{theme}.png").convert("RGBA")
    bg.paste(fg, (0, 0), fg)
    bg.save(f"mac-studio-{theme}.png", compress_level=9)
    print(theme)
