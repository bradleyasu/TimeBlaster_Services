#!/usr/bin/env python3
"""Generate Timeblaster's binary image assets.

The drawing primitives live in tbdisplay.py, which timeblaster-splash shares, so
the boot splash and the television's standby screen are drawn with the same font
and palette and cannot drift apart.

Run it via `make assets`; setup.sh also runs it when an asset is missing.
"""

import argparse
import math
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

from tbdisplay import (  # noqa: E402
    BG, DIM, EDGE, FAINT, GLYPH_H, GLYPH_W, GREEN, PANEL, RED, Canvas,
)

# The JoC-TV splat, in a 512x512 space.
#
# One smooth closed outline, not a union of circles. Circles were the first
# attempt, because Canvas could already draw them -- but a union scallops at
# every intersection and the tapered arms end in points, so it read as jagged
# rather than as thrown paint. Canvas.polygon exists to let this be a curve.
#
# The outline is a radius that varies with angle, perturbed by a few low
# harmonics. Low is the important part: harmonics 2, 3 and 5 give big rounded
# lobes, and everything stays gently curved because nothing higher is loud
# enough to pinch the outline into a spike.
SPLAT_ORANGE = (247, 129, 13)
SPLAT_INK = (255, 255, 255)

SPLAT_CX, SPLAT_CY, SPLAT_R = 252.0, 240.0, 142.0

# (harmonic, amplitude, phase). Phases are irregular on purpose: round numbers
# line the lobes up and the result looks manufactured.
SPLAT_HARMONICS = [
    (2, 0.055, 0.60),
    (3, 0.085, 2.35),
    (5, 0.105, 4.10),
    (7, 0.075, 1.15),
    (11, 0.045, 5.30),
]

# Flung clear of the mass. Genuinely round, so circles are right for these.
SPLAT_DROPS = [
    (86, 132, 17), (432, 122, 13), (458, 306, 10),
    (62, 338, 12), (398, 86, 8), (120, 74, 7), (470, 196, 7),
]


def splat_radius(theta):
    r = 1.0
    for n, amp, phase in SPLAT_HARMONICS:
        r += amp * math.sin(n * theta + phase)
    return SPLAT_R * r


def splat_points(count):
    """Sample the outline. Dense for the raster fill, sparse for the vector."""
    pts = []
    for i in range(count):
        t = 2.0 * math.pi * i / count
        r = splat_radius(t)
        pts.append((SPLAT_CX + r * math.cos(t), SPLAT_CY + r * math.sin(t)))
    return pts


def make_icon(path, size):
    """The app icon: the JoC-TV splat over the app's near-black."""
    c = Canvas(size, size, BG)
    u = size / 512.0

    def s(v):
        return max(1, int(round(v * u)))

    c.rounded_rect(0, 0, size, size, s(96), BG)
    # Sampled finely enough that each segment is well under a pixel, so the
    # fill is a curve rather than a polygon anyone can count the sides of.
    c.polygon([(x * u, y * u) for x, y in splat_points(2000)], SPLAT_ORANGE)
    for cx, cy, r in SPLAT_DROPS:
        c.circle(s(cx), s(cy), s(r), SPLAT_ORANGE)

    # Sized to sit inside the mass rather than spill onto the black: at 512
    # the wordmark is 245px wide against roughly 290px of orange at that line.
    draw_wordmark(c, s(244), s(246), max(1, int(round(7 * u))))

    sub = max(1, int(round(4 * u)))
    c.center_text("TIMEBLASTER", s(432), sub, SPLAT_ORANGE, spacing=3)

    print(f"wrote {path} ({c.w}x{c.h}, {c.write_png(path)} bytes)")


def make_maskable_icon(path, size=512):
    """A maskable variant, drawn inside the safe zone.

    Android crops a maskable icon to whatever shape the launcher uses, and only
    the middle 80% is guaranteed to survive. The ordinary icon puts flung
    droplets near the corners and TIMEBLASTER at 88% down the canvas, so
    declaring it maskable would have promised a mark that gets its subtitle
    sliced off. This one scales the art to 72% and drops the wordmark's
    subtitle, which is unreadable at launcher sizes anyway.
    """
    c = Canvas(size, size, BG)
    u = size / 512.0
    inset = 0.72

    def s(v):
        # Scale about the centre so the whole mark lands inside the safe zone.
        return max(1, int(round((256 + (v - 256) * inset) * u)))

    def sr(v):
        return max(1, int(round(v * inset * u)))

    c.rect(0, 0, size, size, BG)
    c.polygon(
        [(s(x), s(y)) for x, y in splat_points(2000)], SPLAT_ORANGE
    )
    for cx, cy, r in SPLAT_DROPS:
        c.circle(s(cx), s(cy), sr(r), SPLAT_ORANGE)
    draw_wordmark(c, s(244), s(246), max(1, int(round(7 * inset * u))))

    print(f"wrote {path} ({c.w}x{c.h}, {c.write_png(path)} bytes)")


def make_icon_svg(path):
    """The same splat as a vector, for browsers that prefer one.

    Written from the same circle lists as the PNGs rather than drawn by hand,
    so the two cannot drift apart. The lettering is the one difference: SVG has
    real type available, where the PNGs are limited to the shared 5x7 bitmap
    font. Both use a monospace face so they still read as the same mark.
    """
    # A quadratic through the midpoints of a coarse sampling: C1-continuous, so
    # it is genuinely curved at any zoom rather than a polygon with enough sides
    # to pass at icon size. Far fewer points than the raster fill needs.
    pts = splat_points(96)
    n = len(pts)

    def mid(a, b):
        return ((a[0] + b[0]) / 2.0, (a[1] + b[1]) / 2.0)

    m0 = mid(pts[-1], pts[0])
    d = ["M%.1f %.1f" % m0]
    for i in range(n):
        ctrl = pts[i]
        end = mid(pts[i], pts[(i + 1) % n])
        d.append("Q%.1f %.1f %.1f %.1f" % (ctrl[0], ctrl[1], end[0], end[1]))
    d.append("Z")

    circles = '<path d="%s"/>' % "".join(d)
    circles += "".join(
        f'<circle cx="{cx}" cy="{cy}" r="{r}"/>' for cx, cy, r in SPLAT_DROPS
    )
    orange = "#%02x%02x%02x" % SPLAT_ORANGE
    mono = "ui-monospace, SFMono-Regular, Menlo, Consolas, monospace"

    svg = (
        '<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 512 512" '
        'role="img" aria-label="JoC-TV Timeblaster">'
        '<rect width="512" height="512" rx="96" fill="#07090a"/>'
        f'<g fill="{orange}">{circles}</g>'
        # textLength pins the width rather than trusting a font to be present
        # and to have the metrics assumed here. Whatever face the device
        # actually resolves, the wordmark stays inside the mass.
        f'<text x="244" y="270" text-anchor="middle" font-family="{mono}" '
        'font-size="68" font-weight="700" fill="#ffffff" '
        'textLength="245" lengthAdjust="spacingAndGlyphs">JoC-TV</text>'
        f'<text x="256" y="452" text-anchor="middle" font-family="{mono}" '
        f'font-size="30" font-weight="700" fill="{orange}" '
        'textLength="284" lengthAdjust="spacingAndGlyphs">'
        'TIMEBLASTER</text>'
        '</svg>\n'
    )
    with open(path, "w", encoding="utf-8") as fh:
        fh.write(svg)
    print(f"wrote {path} ({len(svg)} bytes)")


def draw_wordmark(c, cx, cy, scale):
    """Draw "JoC-TV" centred on (cx, cy).

    The shared font is uppercase-only and text() folds case, so the lowercase o
    is drawn as a ring instead of a glyph. It is the only letter of the wordmark
    that is not uppercase, and rendering it as JOC-TV would be a different name.
    """
    advance = (GLYPH_W + 1) * scale
    width = 6 * advance - scale
    left = cx - width // 2
    top = cy - (GLYPH_H * scale) // 2

    c.text("J", left, top, scale, SPLAT_INK)

    # The o: x-height, sitting on the baseline like the rest of the line.
    ox = left + advance + (GLYPH_W * scale) // 2
    oy = top + int(round(4.5 * scale))
    c.circle(ox, oy, int(round(2.4 * scale)), SPLAT_INK)
    c.circle(ox, oy, int(round(1.1 * scale)), SPLAT_ORANGE)

    c.text("C-TV", left + 2 * advance, top, scale, SPLAT_INK)


def draw_standby(c, headline, lines):
    """The shared layout for the fullscreen television screens.

    Kept in one place so the boot splash and the standby screen are visibly the
    same product rather than two screens that happen to be green.
    """
    c.scanlines()

    scale = max(1, c.h // 90)
    c.center_text(headline, c.h // 2 - scale * 16, scale, GREEN)

    # A rule under the wordmark, sized to the text.
    rule_w = Canvas.text_width(headline, scale)
    c.rect((c.w - rule_w) // 2, c.h // 2 - scale * 2, rule_w, max(1, scale // 6), EDGE)

    sub = max(1, scale // 2)
    y = c.h // 2 + scale * 4
    for i, (line, colour) in enumerate(lines):
        c.center_text(line, y, sub, colour)
        y += sub * 14
        _ = i


def make_no_channel(path, width=1920, height=1080):
    """Shown whenever no channel is selected.

    It is what keeps a Linux console off the television, so it is deliberately
    plain, dark (no burn-in risk on a set left on all night) and unambiguous.
    """
    c = Canvas(width, height, BG)
    draw_standby(c, "TIMEBLASTER", [
        ("NO CHANNEL SELECTED", DIM),
        ("TURN THE CHANNEL KNOB", FAINT),
    ])
    print(f"wrote {path} ({c.w}x{c.h}, {c.write_png(path)} bytes)")


def make_booting(path, width=1920, height=1080):
    """Shown from early boot until the daemon has the television under control.

    The same screen is painted straight onto the framebuffer by
    timeblaster-splash; this PNG is the copy mpv falls back to if the
    framebuffer is unavailable.
    """
    c = Canvas(width, height, BG)
    draw_standby(c, "TIMEBLASTER", [
        ("BOOTING, PLEASE STAND BY...", DIM),
    ])
    print(f"wrote {path} ({c.w}x{c.h}, {c.write_png(path)} bytes)")


def main():
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--web-assets", default="internal/web/static/assets",
                    help="where the PWA icons are written")
    ap.add_argument("--deploy-assets", default="deploy/assets",
                    help="where the television assets are written")
    args = ap.parse_args()

    make_icon_svg(os.path.join(args.web_assets, "icon.svg"))
    make_icon(os.path.join(args.web_assets, "icon-180.png"), 180)
    make_icon(os.path.join(args.web_assets, "icon-512.png"), 512)
    make_maskable_icon(os.path.join(args.web_assets, "icon-maskable.png"))
    make_no_channel(os.path.join(args.deploy_assets, "no-channel.png"))
    make_booting(os.path.join(args.deploy_assets, "booting.png"))


if __name__ == "__main__":
    main()
