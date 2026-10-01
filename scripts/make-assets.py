#!/usr/bin/env python3
"""Generate Timeblaster's binary image assets.

Two sources. The PWA icons are derived from the master logo in assets/brand,
because that artwork is hand-made and redrawing it in primitives would be a copy
that drifts from it. The television screens are drawn with the primitives in
tbdisplay.py, which timeblaster-splash shares, so the boot splash and the
standby screen use the same font and palette and cannot drift apart.

Run it via `make assets`; setup.sh also runs it when an asset is missing.
"""

import argparse
import os
import struct
import sys
import zlib

SCRIPT_DIR = os.path.dirname(os.path.abspath(__file__))
REPO_ROOT = os.path.dirname(SCRIPT_DIR)

sys.path.insert(0, SCRIPT_DIR)

from tbdisplay import (  # noqa: E402
    BG, DIM, EDGE, FAINT, GREEN, Canvas,
)

# --- Brand artwork ---------------------------------------------------------
#
# The PWA icons are derived from one master image rather than drawn, because the
# logo is hand-made artwork now and redrawing it in primitives would be a copy
# that drifts. Everything below is stdlib: the Pi has no image library, and
# `make assets` has to run there as well as on a developer machine.


def read_png(path):
    """Decode an 8-bit RGB or RGBA PNG into (width, height, rgba bytearray)."""
    with open(path, "rb") as fh:
        data = fh.read()
    if data[:8] != b"\x89PNG\r\n\x1a\n":
        raise ValueError(f"{path}: not a PNG")

    width = height = depth = colour = None
    idat = bytearray()
    pos = 8
    while pos < len(data):
        (length,) = struct.unpack(">I", data[pos:pos + 4])
        tag = data[pos + 4:pos + 8]
        body = data[pos + 8:pos + 8 + length]
        pos += 12 + length  # length + tag + body + crc
        if tag == b"IHDR":
            width, height, depth, colour = struct.unpack(">IIBB", body[:10])
            if body[10:13] != b"\x00\x00\x00":
                raise ValueError(f"{path}: interlaced or filtered PNGs are not supported")
        elif tag == b"IDAT":
            idat.extend(body)
        elif tag == b"IEND":
            break

    if depth != 8 or colour not in (2, 6):
        raise ValueError(f"{path}: need an 8-bit RGB or RGBA PNG, got depth {depth} colour {colour}")

    channels = 4 if colour == 6 else 3
    raw = zlib.decompress(bytes(idat))
    stride = width * channels
    out = bytearray(width * height * 4)
    prev = bytearray(stride)
    src = 0
    for y in range(height):
        filt = raw[src]
        src += 1
        line = bytearray(raw[src:src + stride])
        src += stride
        # Undo the per-scanline filter. a = left, b = above, c = above-left.
        for i in range(stride):
            a = line[i - channels] if i >= channels else 0
            b = prev[i]
            c = prev[i - channels] if i >= channels else 0
            if filt == 1:
                line[i] = (line[i] + a) & 0xFF
            elif filt == 2:
                line[i] = (line[i] + b) & 0xFF
            elif filt == 3:
                line[i] = (line[i] + ((a + b) >> 1)) & 0xFF
            elif filt == 4:
                pa, pb, pc = abs(b - c), abs(a - c), abs(a + b - 2 * c)
                pred = a if (pa <= pb and pa <= pc) else (b if pb <= pc else c)
                line[i] = (line[i] + pred) & 0xFF
            elif filt != 0:
                raise ValueError(f"{path}: unknown filter {filt} on row {y}")
        prev = line

        dst = y * width * 4
        if channels == 4:
            out[dst:dst + width * 4] = line
        else:
            for x in range(width):
                o, i = dst + x * 4, x * 3
                out[o:o + 3] = line[i:i + 3]
                out[o + 3] = 255
    return width, height, out


def resize_rgba(src, sw, sh, dw, dh):
    """Box-filter an RGBA buffer down to dw x dh.

    A box filter rather than nearest: the icons are a large reduction, and
    point-sampling an image with fine white lettering produces broken strokes.
    """
    out = bytearray(dw * dh * 4)
    for dy in range(dh):
        y0, y1 = dy * sh // dh, max(dy * sh // dh + 1, (dy + 1) * sh // dh)
        for dx in range(dw):
            x0, x1 = dx * sw // dw, max(dx * sw // dw + 1, (dx + 1) * sw // dw)
            r = g = b = a = n = 0
            for y in range(y0, y1):
                row = y * sw * 4
                for x in range(x0, x1):
                    i = row + x * 4
                    r += src[i]; g += src[i + 1]; b += src[i + 2]; a += src[i + 3]
                    n += 1
            o = (dy * dw + dx) * 4
            out[o] = r // n
            out[o + 1] = g // n
            out[o + 2] = b // n
            out[o + 3] = a // n
    return out


def corner_colour(src, w, h, inset=4):
    """Average the four corners, used to extend the artwork to a square.

    The logo is landscape and the icons are square, so the sides have to be
    filled with something. The artwork fades to a near-black vignette at its
    corners, so continuing that colour reads as one image rather than as a
    letterboxed photograph.
    """
    total = [0, 0, 0]
    pts = [(inset, inset), (w - 1 - inset, inset),
           (inset, h - 1 - inset), (w - 1 - inset, h - 1 - inset)]
    for x, y in pts:
        i = (y * w + x) * 4
        for c in range(3):
            total[c] += src[i + c]
    return tuple(v // len(pts) for v in total)


_art_cache = {}


def load_art(path):
    if path not in _art_cache:
        _art_cache[path] = read_png(path)
    return _art_cache[path]


def make_brand_icon(path, art_path, size, coverage=1.0):
    """Render the master artwork into a square icon of the given size.

    coverage is how much of the icon's width the artwork spans. Maskable icons
    pass less than 1 so the logo stays inside the safe zone that launchers may
    crop a circle out of.
    """
    sw, sh, src = load_art(art_path)
    bg = corner_colour(src, sw, sh)
    c = Canvas(size, size, background=bg)

    dw = max(1, int(round(size * coverage)))
    dh = max(1, int(round(dw * sh / sw)))
    if dh > size:  # never taller than the icon
        dh = size
        dw = max(1, int(round(dh * sw / sh)))
    scaled = resize_rgba(src, sw, sh, dw, dh)

    ox, oy = (size - dw) // 2, (size - dh) // 2
    for y in range(dh):
        for x in range(dw):
            i = (y * dw + x) * 4
            alpha = scaled[i + 3]
            if alpha == 0:
                continue
            if alpha == 255:
                c.set(ox + x, oy + y, (scaled[i], scaled[i + 1], scaled[i + 2]))
            else:
                # Composite over the background rather than onto black, or a
                # soft edge would show as a dark halo against the vignette.
                c.set(ox + x, oy + y, tuple(
                    (scaled[i + k] * alpha + bg[k] * (255 - alpha)) // 255 for k in range(3)))

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
    # Resolved against the repository rather than the working directory: the
    # output paths are overridden by whoever calls this, but the artwork is an
    # input that has to be found, and setup.sh runs the script from elsewhere.
    ap.add_argument("--brand-art",
                    default=os.path.join(REPO_ROOT, "assets", "brand", "joctv-timeblaster.png"),
                    help="master logo the PWA icons are derived from")
    args = ap.parse_args()

    # Icons come from the master artwork. The drawn splat below it is kept for
    # the television screens, which are rendered at display resolution.
    art = args.brand_art
    if not os.path.exists(art):
        sys.exit(f"brand artwork not found: {art}\n"
                 f"Pass --brand-art, or restore it from the repository.")
    make_brand_icon(os.path.join(args.web_assets, "icon-180.png"), art, 180)
    make_brand_icon(os.path.join(args.web_assets, "icon-192.png"), art, 192)
    make_brand_icon(os.path.join(args.web_assets, "icon-512.png"), art, 512)
    # A launcher may crop a circle out of a maskable icon, so the logo is held
    # well inside the safe zone and only the background reaches the edges.
    make_brand_icon(os.path.join(args.web_assets, "icon-maskable.png"), art, 512,
                    coverage=0.68)
    make_no_channel(os.path.join(args.deploy_assets, "no-channel.png"))
    make_booting(os.path.join(args.deploy_assets, "booting.png"))


if __name__ == "__main__":
    main()
