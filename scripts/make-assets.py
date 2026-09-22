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
    BG, DIM, EDGE, FAINT, GREEN, PANEL, RED, Canvas,
)


def make_icon(path, size):
    """The app icon: a Timeblaster front panel showing 6:30."""
    c = Canvas(size, size, BG)
    u = size / 512.0

    def s(v):
        return max(1, int(round(v * u)))

    c.rounded_rect(0, 0, size, size, s(96), (11, 15, 12))
    c.rounded_rect(s(56), s(140), s(400), s(232), s(28), EDGE)
    c.rounded_rect(s(64), s(148), s(384), s(216), s(24), PANEL)

    scale = max(1, int(round(18 * u)))
    w = Canvas.text_width("6:30", scale)
    c.text("6:30", (size - w) // 2, s(196), scale, GREEN)

    c.circle(s(126), s(410), s(26), RED)
    c.circle(s(256), s(410), s(18), DIM)
    c.circle(s(386), s(410), s(18), DIM)

    print(f"wrote {path} ({c.w}x{c.h}, {c.write_png(path)} bytes)")


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

    make_icon(os.path.join(args.web_assets, "icon-180.png"), 180)
    make_icon(os.path.join(args.web_assets, "icon-512.png"), 512)
    make_no_channel(os.path.join(args.deploy_assets, "no-channel.png"))
    make_booting(os.path.join(args.deploy_assets, "booting.png"))


if __name__ == "__main__":
    main()
