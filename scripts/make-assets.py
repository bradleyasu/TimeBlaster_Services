#!/usr/bin/env python3
"""Generate Timeblaster's binary image assets.

The drawing primitives live in tbdisplay.py, which timeblaster-splash shares, so
the boot splash and the television's standby screen are drawn with the same font
and palette and cannot drift apart.

Run it via `make assets`; setup.sh also runs it when an asset is missing.
"""

import argparse
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

from tbdisplay import (  # noqa: E402
    BG, DIM, EDGE, FAINT, GLYPH_H, GLYPH_W, GREEN, PANEL, RED, Canvas,
)

# The JoC-TV splat, in a 512x512 space.
#
# Defined as a union of circles rather than a curve because there is no imaging
# library on the build machine -- only the hand-rolled Canvas in tbdisplay.py,
# which has no path filling. Circles are the one primitive this and the SVG can
# draw identically, so the two renderings cannot drift apart.
#
# What separates a splat from a cloud is taper and asymmetry: arms of shrinking
# circles flung different distances, rather than evenly spaced bumps of equal
# size. The first attempt at this used uniform lobes and read as a cloud.
SPLAT_ORANGE = (247, 129, 13)
SPLAT_INK = (255, 255, 255)

_CORE = [
    (250, 236, 96),
    (322, 198, 58),
    (182, 288, 52),
    (296, 300, 46),
]

# angle, reach, radius at the root, radius at the tip, how many circles
_ARMS = [
    (18, 186, 44, 7, 7),
    (68, 126, 34, 6, 6),
    (112, 168, 40, 5, 7),
    (158, 150, 38, 7, 6),
    (203, 196, 42, 6, 8),
    (248, 118, 30, 5, 5),
    (292, 172, 36, 6, 7),
    (334, 138, 32, 8, 6),
]

# Flung clear of the mass. These are what make it read as thrown paint.
SPLAT_DROPS = [
    (86, 132, 17), (432, 122, 13), (458, 306, 10),
    (62, 338, 12), (398, 86, 8), (120, 74, 7), (470, 196, 7),
]


def _splat_body():
    """The mass, as circles. Hand-tuned angles and reaches, not a loop over a
    regular polygon -- evenly spaced arms look manufactured."""
    import math

    out = list(_CORE)
    cx, cy = 250, 236
    for deg, reach, r0, r1, n in _ARMS:
        a = math.radians(deg)
        for i in range(1, n + 1):
            t = i / float(n)
            out.append((
                int(round(cx + math.cos(a) * reach * t)),
                int(round(cy + math.sin(a) * reach * t)),
                int(round(r0 + (r1 - r0) * t)),
            ))
    return out


SPLAT_BODY = _splat_body()


def make_icon(path, size):
    """The app icon: the JoC-TV splat over the app's near-black."""
    c = Canvas(size, size, BG)
    u = size / 512.0

    def s(v):
        return max(1, int(round(v * u)))

    c.rounded_rect(0, 0, size, size, s(96), BG)
    for cx, cy, r in SPLAT_BODY + SPLAT_DROPS:
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
    for cx, cy, r in SPLAT_BODY + SPLAT_DROPS:
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
    circles = "".join(
        f'<circle cx="{cx}" cy="{cy}" r="{r}"/>'
        for cx, cy, r in SPLAT_BODY + SPLAT_DROPS
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
    print(f"wrote {path} ({len(svg)} bytes, {len(SPLAT_BODY) + len(SPLAT_DROPS)} circles)")


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
