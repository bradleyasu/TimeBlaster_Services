#!/usr/bin/env python3
"""Generate Timeblaster's binary image assets.

The project deliberately has no image-library dependency and no checked-in
binaries that nobody can regenerate. This script writes PNGs directly (zlib plus
the PNG chunk format) and draws text with a built-in 5x7 bitmap font, which suits
the device's late-1990s aesthetic far better than an anti-aliased system font
would.

Run it via `make assets`; setup.sh also runs it when an asset is missing.
"""

import argparse
import os
import struct
import zlib

# --- 5x7 bitmap font ---------------------------------------------------------
# Each glyph is seven rows of five bits, MSB first. Only the characters the
# generated assets need are defined; unknown characters render as a blank.
FONT = {
    ' ': ["00000"] * 7,
    '-': ["00000", "00000", "00000", "11111", "00000", "00000", "00000"],
    ':': ["00000", "00100", "00100", "00000", "00100", "00100", "00000"],
    '.': ["00000", "00000", "00000", "00000", "00000", "00110", "00110"],
    '0': ["01110", "10001", "10011", "10101", "11001", "10001", "01110"],
    '1': ["00100", "01100", "00100", "00100", "00100", "00100", "01110"],
    '2': ["01110", "10001", "00001", "00010", "00100", "01000", "11111"],
    '3': ["11111", "00010", "00100", "00010", "00001", "10001", "01110"],
    '4': ["00010", "00110", "01010", "10010", "11111", "00010", "00010"],
    '5': ["11111", "10000", "11110", "00001", "00001", "10001", "01110"],
    '6': ["00110", "01000", "10000", "11110", "10001", "10001", "01110"],
    '7': ["11111", "00001", "00010", "00100", "01000", "01000", "01000"],
    '8': ["01110", "10001", "10001", "01110", "10001", "10001", "01110"],
    '9': ["01110", "10001", "10001", "01111", "00001", "00010", "01100"],
    'A': ["01110", "10001", "10001", "11111", "10001", "10001", "10001"],
    'B': ["11110", "10001", "10001", "11110", "10001", "10001", "11110"],
    'C': ["01110", "10001", "10000", "10000", "10000", "10001", "01110"],
    'D': ["11110", "10001", "10001", "10001", "10001", "10001", "11110"],
    'E': ["11111", "10000", "10000", "11110", "10000", "10000", "11111"],
    'F': ["11111", "10000", "10000", "11110", "10000", "10000", "10000"],
    'G': ["01110", "10001", "10000", "10111", "10001", "10001", "01111"],
    'H': ["10001", "10001", "10001", "11111", "10001", "10001", "10001"],
    'I': ["01110", "00100", "00100", "00100", "00100", "00100", "01110"],
    'J': ["00111", "00010", "00010", "00010", "00010", "10010", "01100"],
    'K': ["10001", "10010", "10100", "11000", "10100", "10010", "10001"],
    'L': ["10000", "10000", "10000", "10000", "10000", "10000", "11111"],
    'M': ["10001", "11011", "10101", "10101", "10001", "10001", "10001"],
    'N': ["10001", "10001", "11001", "10101", "10011", "10001", "10001"],
    'O': ["01110", "10001", "10001", "10001", "10001", "10001", "01110"],
    'P': ["11110", "10001", "10001", "11110", "10000", "10000", "10000"],
    'Q': ["01110", "10001", "10001", "10001", "10101", "10010", "01101"],
    'R': ["11110", "10001", "10001", "11110", "10100", "10010", "10001"],
    'S': ["01111", "10000", "10000", "01110", "00001", "00001", "11110"],
    'T': ["11111", "00100", "00100", "00100", "00100", "00100", "00100"],
    'U': ["10001", "10001", "10001", "10001", "10001", "10001", "01110"],
    'V': ["10001", "10001", "10001", "10001", "10001", "01010", "00100"],
    'W': ["10001", "10001", "10001", "10101", "10101", "11011", "10001"],
    'X': ["10001", "10001", "01010", "00100", "01010", "10001", "10001"],
    'Y': ["10001", "10001", "01010", "00100", "00100", "00100", "00100"],
    'Z': ["11111", "00001", "00010", "00100", "01000", "10000", "11111"],
}

GLYPH_W, GLYPH_H = 5, 7


class Canvas:
    """A minimal RGB raster with just the primitives these assets need."""

    def __init__(self, width, height, background=(0, 0, 0)):
        self.w, self.h = width, height
        self.px = bytearray(bytes(background) * (width * height))

    def set(self, x, y, colour):
        if 0 <= x < self.w and 0 <= y < self.h:
            i = (y * self.w + x) * 3
            self.px[i:i + 3] = bytes(colour)

    def rect(self, x, y, w, h, colour):
        for yy in range(y, y + h):
            for xx in range(x, x + w):
                self.set(xx, yy, colour)

    def rounded_rect(self, x, y, w, h, radius, colour):
        r2 = radius * radius
        for yy in range(y, y + h):
            for xx in range(x, x + w):
                # Only the four corners need the distance test.
                cx = x + radius if xx < x + radius else (x + w - 1 - radius if xx > x + w - 1 - radius else xx)
                cy = y + radius if yy < y + radius else (y + h - 1 - radius if yy > y + h - 1 - radius else yy)
                if (xx - cx) ** 2 + (yy - cy) ** 2 <= r2:
                    self.set(xx, yy, colour)

    def circle(self, cx, cy, r, colour):
        for yy in range(cy - r, cy + r + 1):
            for xx in range(cx - r, cx + r + 1):
                if (xx - cx) ** 2 + (yy - cy) ** 2 <= r * r:
                    self.set(xx, yy, colour)

    def text(self, s, x, y, scale, colour, spacing=1):
        """Draw s with its top-left corner at (x, y). Returns the width drawn."""
        cursor = x
        for ch in s.upper():
            glyph = FONT.get(ch, FONT[' '])
            for row, bits in enumerate(glyph):
                for col, bit in enumerate(bits):
                    if bit == '1':
                        self.rect(cursor + col * scale, y + row * scale, scale, scale, colour)
            cursor += (GLYPH_W + spacing) * scale
        return cursor - x - spacing * scale

    @staticmethod
    def text_width(s, scale, spacing=1):
        if not s:
            return 0
        return len(s) * (GLYPH_W + spacing) * scale - spacing * scale

    def center_text(self, s, y, scale, colour, spacing=1):
        w = self.text_width(s, scale, spacing)
        self.text(s, (self.w - w) // 2, y, scale, colour, spacing)

    def write_png(self, path):
        raw = bytearray()
        stride = self.w * 3
        for y in range(self.h):
            raw.append(0)  # filter type 0 (None): simple and plenty small enough
            raw.extend(self.px[y * stride:(y + 1) * stride])

        def chunk(tag, data):
            out = struct.pack(">I", len(data)) + tag + data
            return out + struct.pack(">I", zlib.crc32(tag + data) & 0xFFFFFFFF)

        png = b"\x89PNG\r\n\x1a\n"
        png += chunk(b"IHDR", struct.pack(">IIBBBBB", self.w, self.h, 8, 2, 0, 0, 0))
        png += chunk(b"IDAT", zlib.compress(bytes(raw), 9))
        png += chunk(b"IEND", b"")

        os.makedirs(os.path.dirname(os.path.abspath(path)), exist_ok=True)
        with open(path, "wb") as f:
            f.write(png)
        print(f"wrote {path} ({self.w}x{self.h}, {len(png)} bytes)")


BG = (7, 9, 10)
PANEL = (16, 21, 18)
EDGE = (34, 48, 38)
GREEN = (51, 255, 51)
DIM = (31, 170, 31)
RED = (255, 68, 68)


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
    c.write_png(path)


def make_no_channel(path, width=1920, height=1080):
    """The fullscreen image shown whenever no channel is selected.

    It is what keeps a Linux console off the television, so it is deliberately
    plain, dark (no burn-in risk on an OLED left on all night) and unambiguous.
    """
    c = Canvas(width, height, BG)

    # A subtle scanline texture, because this is a 1990s television appliance.
    for y in range(0, height, 4):
        c.rect(0, y, width, 1, (10, 13, 11))

    scale = max(1, height // 90)
    c.center_text("TIMEBLASTER", height // 2 - scale * 16, scale, GREEN)

    sub = max(1, scale // 2)
    c.center_text("NO CHANNEL SELECTED", height // 2 + scale * 4, sub, DIM)
    c.center_text("TURN THE CHANNEL KNOB", height // 2 + scale * 4 + sub * 14, sub, (60, 95, 65))

    # A rule under the wordmark, sized to the text.
    rule_w = Canvas.text_width("TIMEBLASTER", scale)
    c.rect((width - rule_w) // 2, height // 2 - scale * 2, rule_w, max(1, scale // 6), EDGE)

    c.write_png(path)


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


if __name__ == "__main__":
    main()
