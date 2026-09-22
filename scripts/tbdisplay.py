"""Timeblaster's drawing primitives: a bitmap font, a raster canvas, and the
two things it gets painted onto.

The project deliberately has no image-library dependency and no checked-in
binaries that nobody can regenerate. This module writes PNGs directly (zlib plus
the PNG chunk format) and paints the Linux framebuffer directly, drawing text
with a built-in 5x7 bitmap font that suits the device's late-1990s aesthetic far
better than an anti-aliased system font would.

Two consumers share it:

  make-assets.py      generates the PWA icons and the television images
  timeblaster-splash  paints the boot message straight onto the framebuffer

setup.sh installs this file alongside timeblaster-splash so the splash has it at
runtime.
"""

import os
import struct
import zlib

# --- 5x7 bitmap font ---------------------------------------------------------
# Each glyph is seven rows of five bits, MSB first. Only the characters the
# generated assets and the boot splash need are defined; anything else renders
# as a blank rather than as garbage.
FONT = {
    ' ': ["00000"] * 7,
    '-': ["00000", "00000", "00000", "11111", "00000", "00000", "00000"],
    ':': ["00000", "00100", "00100", "00000", "00100", "00100", "00000"],
    '.': ["00000", "00000", "00000", "00000", "00000", "00110", "00110"],
    ',': ["00000", "00000", "00000", "00000", "00110", "00110", "01100"],
    '!': ["00100", "00100", "00100", "00100", "00100", "00000", "00100"],
    '/': ["00001", "00010", "00010", "00100", "01000", "01000", "10000"],
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

# --- Palette -----------------------------------------------------------------
# The same phosphor green as the companion app and the channel-change banner.
BG = (7, 9, 10)
PANEL = (16, 21, 18)
EDGE = (34, 48, 38)
GREEN = (51, 255, 51)
DIM = (31, 170, 31)
FAINT = (60, 95, 65)
RED = (255, 68, 68)


class Canvas:
    """A minimal RGB raster with just the primitives these assets need."""

    def __init__(self, width, height, background=BG):
        self.w, self.h = width, height
        self.px = bytearray(bytes(background) * (width * height))

    def set(self, x, y, colour):
        if 0 <= x < self.w and 0 <= y < self.h:
            i = (y * self.w + x) * 3
            self.px[i:i + 3] = bytes(colour)

    def rect(self, x, y, w, h, colour):
        # Filled with one slice assignment per row rather than a call per pixel.
        # This runs on a Raspberry Pi during boot, where the naive version costs
        # seconds on a 1080p screen and the splash arrives after the thing it is
        # meant to cover up.
        x0, y0 = max(0, x), max(0, y)
        x1, y1 = min(self.w, x + w), min(self.h, y + h)
        if x1 <= x0 or y1 <= y0:
            return
        span = bytes(colour) * (x1 - x0)
        for yy in range(y0, y1):
            i = (yy * self.w + x0) * 3
            self.px[i:i + len(span)] = span

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

    def scanlines(self, colour=(10, 13, 11), step=4):
        """A faint horizontal texture, because this is a 1990s television."""
        for y in range(0, self.h, step):
            self.rect(0, y, self.w, 1, colour)

    # --- Output --------------------------------------------------------------

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

        directory = os.path.dirname(os.path.abspath(path))
        if directory:
            os.makedirs(directory, exist_ok=True)
        with open(path, "wb") as f:
            f.write(png)
        return len(png)

    def to_framebuffer_bytes(self, bpp, stride):
        """Pack the canvas for a Linux framebuffer of the given depth.

        Supports the two formats a Raspberry Pi actually presents: 32-bit
        XRGB8888 (stored little-endian, so byte order is B, G, R, X) and 16-bit
        RGB565. `stride` is the framebuffer's line length in bytes, which is not
        always width * bytes-per-pixel.

        The 32-bit path deinterleaves with extended slice assignment rather than
        a per-pixel loop, which is the difference between a boot splash that
        appears promptly and one that arrives after the boot it was covering.
        """
        if bpp not in (16, 32):
            raise ValueError(f"unsupported framebuffer depth: {bpp} bits per pixel")

        out = bytearray(stride * self.h)
        width = self.w
        src_stride = width * 3

        if bpp == 32:
            opaque = b"\xff" * width
            for y in range(self.h):
                rgb = self.px[y * src_stride:(y + 1) * src_stride]
                row = bytearray(width * 4)
                row[0::4] = rgb[2::3]   # B
                row[1::4] = rgb[1::3]   # G
                row[2::4] = rgb[0::3]   # R
                row[3::4] = opaque      # X
                o = y * stride
                out[o:o + width * 4] = row
        else:
            for y in range(self.h):
                rgb = self.px[y * src_stride:(y + 1) * src_stride]
                row = bytearray()
                for r, g, b in zip(rgb[0::3], rgb[1::3], rgb[2::3]):
                    v = ((r & 0xF8) << 8) | ((g & 0xFC) << 3) | (b >> 3)
                    row += v.to_bytes(2, "little")
                o = y * stride
                out[o:o + width * 2] = row

        return bytes(out)


# --- Framebuffer discovery ---------------------------------------------------

def framebuffer_geometry(device="fb0"):
    """Read a framebuffer's geometry from sysfs.

    Returns (width, height, bits_per_pixel, stride) or None when the device is
    not present — which is the normal case on a development machine, and can
    also happen on a Pi if fbdev emulation is disabled.
    """
    base = f"/sys/class/graphics/{device}"

    def read(name):
        try:
            with open(os.path.join(base, name)) as f:
                return f.read().strip()
        except OSError:
            return None

    size = read("virtual_size")
    bpp = read("bits_per_pixel")
    stride = read("stride")
    if not size or not bpp:
        return None

    try:
        width, height = (int(v) for v in size.split(","))
        bpp = int(bpp)
    except ValueError:
        return None

    if stride:
        try:
            stride = int(stride)
        except ValueError:
            stride = 0
    else:
        stride = 0
    if stride <= 0:
        stride = width * (bpp // 8)

    return width, height, bpp, stride
