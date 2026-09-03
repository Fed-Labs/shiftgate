#!/usr/bin/env python3
"""Generate the SHIFT desktop app icon as a PNG, stdlib only.

Draws the product mark — the instrument palette's near-black ground with the
single state-transfer accent as a rightward chevron — and encodes it as a
512x512 RGBA PNG. Tauri's bundler for Linux consumes this file directly; for
macOS and Windows installers, `npm run icon` derives the full set from it.

Usage: python3 scripts/make-icon.py
"""

import struct
import zlib

SIZE = 512
GROUND = (10, 10, 11, 255)        # #0a0a0b — palette bg
ACCENT = (20, 224, 176, 255)      # #14e0b0 — the accent
EDGE = (38, 38, 44, 255)          # hairline border


def raster():
    """Sample the mark per pixel: two accent chevrons on the ground."""
    pixels = []
    cx = SIZE / 2
    for y in range(SIZE):
        row = []
        for x in range(SIZE):
            color = GROUND
            # Rounded-rect border.
            margin = 8
            radius = 96
            if border_hit(x, y, margin, radius):
                color = EDGE
            # Two chevrons pointing right, like migration between machines.
            for offset in (-70, 70):
                if chevron_hit(x - cx + offset, y - cx):
                    color = ACCENT
            row.append(color)
        pixels.append(row)
    return pixels


def border_hit(x, y, margin, radius):
    inside = margin < x < SIZE - margin and margin < y < SIZE - margin
    if not inside:
        return False
    near = (
        margin < x < margin + radius or SIZE - margin - radius < x < SIZE - margin
    ) and (
        margin < y < margin + radius or SIZE - margin - radius < y < SIZE - margin
    )
    if not near:
        return False
    # Distance from the nearest corner's arc center.
    cxc = margin + radius if x < SIZE / 2 else SIZE - margin - radius
    cyc = margin + radius if y < SIZE / 2 else SIZE - margin - radius
    distance = ((x - cxc) ** 2 + (y - cyc) ** 2) ** 0.5
    return distance > radius


def chevron_hit(x, y):
    """A '>' stroke of thickness 40, half-height 110."""
    thickness = 40
    half_height = 110
    if abs(y) > half_height:
        return False
    # Leading edge: x = -y * 0.8 (left to center), trailing parallel +thickness.
    lead = -abs(y) * 0.8
    return lead <= x <= lead + thickness


def encode(pixels):
    def chunk(tag, data):
        body = tag + data
        return struct.pack(">I", len(data)) + body + struct.pack(">I", zlib.crc32(body))

    scanlines = b""
    for row in pixels:
        scanlines += b"\x00" + b"".join(struct.pack("BBBB", *pixel) for pixel in row)
    return b"".join(
        [
            b"\x89PNG\r\n\x1a\n",
            chunk(b"IHDR", struct.pack(">IIBBBBB", SIZE, SIZE, 8, 6, 0, 0, 0)),
            chunk(b"IDAT", zlib.compress(scanlines, 9)),
            chunk(b"IEND", b""),
        ]
    )


def main():
    from pathlib import Path

    target = Path(__file__).resolve().parent.parent / "src-tauri" / "icons" / "icon.png"
    target.parent.mkdir(parents=True, exist_ok=True)
    target.write_bytes(encode(raster()))
    print(f"wrote {target} ({target.stat().st_size} bytes)")


if __name__ == "__main__":
    main()
