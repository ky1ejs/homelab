"""Draw the wallpaper's foreground as SVG: the dot grid, the Studio, the name
and the ten area marks, in the kylejs.me design tokens, on a transparent
background. compose.py adds the background.

The marks come from kylejs-site's src/design/marks.ts, and the text is turned
into outlines with fontTools from the site's own Lexend and JetBrains Mono, so
the SVG needs no fonts installed.

Usage: python3 make.py <kylejs-site checkout> <out_dir>
"""
import re
import sys
from pathlib import Path

from fontTools.ttLib import TTFont
from fontTools.varLib import instancer
from fontTools.pens.svgPathPen import SVGPathPen
from fontTools.pens.transformPen import TransformPen

site = Path(sys.argv[1])
out = Path(sys.argv[2])
out.mkdir(parents=True, exist_ok=True)

THEMES = {
    "dark": dict(paper="#121318", ink="#ECEDF1", muted="#8E95A3", rule="#2C2E36",
                 accent="#B575F0", accentText="#CDA3F5", accentSoft="#2A1E3D"),
    "light": dict(paper="#F3F5F8", ink="#12141A", muted="#5F6776", rule="#DDE2E8",
                  accent="#8319E6", accentText="#6914B8", accentSoft="#F1E8FB"),
}

# The ten area marks, read from the site's source of truth.
marks_src = (site / "src/design/marks.ts").read_text()
MARKS = []
for name, body in re.findall(r"\n  (\w+): \{(.*?)\n  \},", marks_src, re.S):
    shape = re.search(r'shape:\s*"([^"]+)"', body).group(1)
    detail = re.search(r'detail:\s*"([^"]+)"', body).group(1)
    MARKS.append((name, shape, detail))
assert len(MARKS) == 10, len(MARKS)

# A Mac Studio, front view, on the same 24 grid: two USB-C ports, an SD slot, the base.
STUDIO_SHAPE = ("M3.5 8h17a1.5 1.5 0 0 1 1.5 1.5v6.5a1.5 1.5 0 0 1 -1.5 1.5"
                "h-17a1.5 1.5 0 0 1 -1.5 -1.5v-6.5a1.5 1.5 0 0 1 1.5 -1.5z")
STUDIO_DETAIL = ("M5.6 13.4h1.5a0.6 0.6 0 0 1 0 1.2h-1.5a0.6 0.6 0 0 1 0 -1.2z "
                 "M9.2 13.4h1.5a0.6 0.6 0 0 1 0 1.2h-1.5a0.6 0.6 0 0 1 0 -1.2z "
                 "M14.6 14h3.8 M5.5 18.75h13")


def font_at(path, wght):
    f = TTFont(path)
    return instancer.instantiateVariableFont(f, {"wght": wght})


def text_path(font, text, size, tracking=0.0):
    """Outline `text` as one SVG path. Returns (d, width) in px at `size`."""
    upm = font["head"].unitsPerEm
    scale = size / upm
    cmap = font.getBestCmap()
    glyphs = font.getGlyphSet()
    hmtx = font["hmtx"]
    x = 0.0
    parts = []
    for ch in text:
        g = cmap[ord(ch)]
        pen = SVGPathPen(glyphs)
        # Font units are y-up; SVG is y-down.
        glyphs[g].draw(TransformPen(pen, (scale, 0, 0, -scale, x, 0)))
        parts.append(pen.getCommands())
        x += hmtx[g][0] * scale + tracking
    return " ".join(parts), x - tracking


fonts = site / "node_modules/@fontsource-variable"
lexend = font_at(fonts / "lexend/files/lexend-latin-wght-normal.woff2", 600)
mono = font_at(fonts / "jetbrains-mono/files/jetbrains-mono-latin-wght-normal.woff2", 500)

W, H = 6016, 3384
cx = W / 2


def mark(shape, detail, x, y, unit, t, filled, stroke, sw=1.75):
    """A mark with its 24 grid's top-left at (x, y), `unit` px per grid unit."""
    fill = t["accentSoft"] if filled else "none"
    return (f'<g transform="translate({x:.1f} {y:.1f}) scale({unit})" fill="none" '
            f'stroke="{stroke}" stroke-width="{sw}" stroke-linecap="round" stroke-linejoin="round">'
            f'<path d="{shape}" fill="{fill}"/><path d="{detail}"/></g>')


def render(theme):
    t = THEMES[theme]
    s = []
    s.append(f'<svg xmlns="http://www.w3.org/2000/svg" width="{W}" height="{H}" viewBox="0 0 {W} {H}">')
    s.append('<defs>'
             # The 24 grid the marks are drawn on, as dots every 3 grid units.
             '<pattern id="grid" width="96" height="96" patternUnits="userSpaceOnUse">'
             f'<circle cx="48" cy="48" r="3" fill="{t["rule"]}"/></pattern>'
             f'<radialGradient id="fade" cx="{cx}" cy="{H/2}" r="{W*0.62:.0f}" gradientUnits="userSpaceOnUse">'
             '<stop offset="0" stop-color="#fff" stop-opacity="1"/>'
             '<stop offset="1" stop-color="#fff" stop-opacity="0.25"/></radialGradient>'
             '<mask id="gridmask"><rect width="100%" height="100%" fill="url(#fade)"/></mask>'
             '</defs>')
    s.append(f'<rect width="{W}" height="{H}" fill="url(#grid)" mask="url(#gridmask)"/>')

    # The Studio, large: 24 grid units at 44px each.
    unit = 44
    s.append(mark(STUDIO_SHAPE, STUDIO_DETAIL, cx - 12 * unit, H * 0.40 - 13 * unit, unit, t,
                  True, t["accent"], sw=0.5))

    # Wordmark and caption.
    d, w = text_path(lexend, "mac-studio", 300, tracking=-6)
    base = H * 0.40 + 13 * unit + 120 + 220
    s.append(f'<path transform="translate({cx - w/2:.1f} {base:.1f})" fill="{t["ink"]}" d="{d}"/>')
    d, w = text_path(mono, "kylejs  /  homelab  /  M5 Max  /  64GB", 68, tracking=4)
    cap = base + 150
    s.append(f'<path transform="translate({cx - w/2:.1f} {cap:.1f})" fill="{t["muted"]}" d="{d}"/>')

    # The ten areas from kylejs.me, in a row, line only.
    size, gap = 104, 88
    row_w = 10 * size + 9 * gap
    y = cap + 230
    for i, (_, shape, detail) in enumerate(MARKS):
        x = cx - row_w / 2 + i * (size + gap)
        s.append(mark(shape, detail, x, y, size / 24, t, False, t["muted"]))

    s.append("</svg>")
    path = out / f"mac-studio-{theme}.svg"
    path.write_text("\n".join(s))
    print(path)


for theme in THEMES:
    render(theme)
