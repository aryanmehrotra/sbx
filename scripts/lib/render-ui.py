#!/usr/bin/env python3
"""Render a captured `sbx ui` frame to SVG.

    render-ui.py <frame.ansi> <out.svg> [alt text]

The dashboard is a full-screen program that paints its own background, so unlike the command
transcript in render-demo.py this has to keep a grid: every cell has a foreground, a
background and a weight, and the painted panel and the selected row are backgrounds rather
than text. The input is the raw bytes of one frame, exactly as the terminal received them.

Nothing here invents anything. The frame is a recording of a real run against real docker, and
this only decides what colour 38;5;77 is.
"""

import html
import re
import sys

# The palette the dashboard actually uses. Anything else falls back to the ordinary
# foreground, which is visibly wrong rather than quietly approximate - the point is to notice.
XTERM = {
    234: "#1c1c1c",  # the panel
    238: "#444444",  # the selected row
    245: "#8a8a8a",  # dim
    77: "#5fd75f",   # green
    80: "#5fd7d7",   # cyan
    179: "#d7af5f",  # amber
    167: "#d75f5f",  # red
}

FG = "#c9d1d9"
BG = "#0d1117"

CELL_W = 7.8  # true advance of ui-monospace/SF Mono at 13px; per-glyph x below pins to this grid
CELL_H = 16.0
PAD_X = 14
PAD_Y = 40  # room for the window chrome

SGR = re.compile(rb"\x1b\[([0-9;]*)m")
OTHER_ESC = re.compile(rb"\x1b\[[0-9;?]*[A-Za-z]")


class Pen:
    def __init__(self):
        self.fg = None
        self.bg = None
        self.bold = False
        self.invert = False

    def copy(self):
        p = Pen()
        p.fg, p.bg, p.bold, p.invert = self.fg, self.bg, self.bold, self.invert
        return p

    def apply(self, params):
        i = 0
        if not params:
            params = [0]
        while i < len(params):
            p = params[i]
            if p == 0:
                self.fg = self.bg = None
                self.bold = self.invert = False
            elif p == 1:
                self.bold = True
            elif p == 7:
                self.invert = True
            elif p == 27:
                self.invert = False
            elif p == 39:
                self.fg = None
            elif p == 49:
                self.bg = None
            elif p in (38, 48) and i + 2 < len(params) and params[i + 1] == 5:
                colour = XTERM.get(params[i + 2])
                if p == 38:
                    self.fg = colour
                else:
                    self.bg = colour
                i += 2
            i += 1

    def colours(self):
        fg = self.fg or FG
        bg = self.bg
        if self.invert:
            fg, bg = bg or BG, fg
        return fg, bg


def parse(raw):
    """Turn one frame into a list of rows of (char, fg, bg, bold)."""
    rows, row, pen = [], [], Pen()
    pos = 0

    while pos < len(raw):
        m = SGR.search(raw, pos)
        if m and m.start() == pos:
            pen.apply([int(x) if x else 0 for x in m.group(1).split(b";")])
            pos = m.end()
            continue

        m2 = OTHER_ESC.match(raw, pos)
        if m2:
            pos = m2.end()
            continue

        end = len(raw)
        for cand in (SGR.search(raw, pos), OTHER_ESC.search(raw, pos)):
            if cand:
                end = min(end, cand.start())

        chunk = raw[pos:end].decode("utf-8", "replace")
        pos = end

        for ch in chunk:
            if ch == "\n":
                rows.append(row)
                row = []
            elif ch == "\r":
                continue
            else:
                fg, bg = pen.colours()
                row.append((ch, fg, bg, pen.bold))

    if row:
        rows.append(row)
    return rows


def runs(cells, key):
    """Group adjacent cells that share key(cell), as (start, text, cell)."""
    out, i = [], 0
    while i < len(cells):
        j = i
        while j < len(cells) and key(cells[j]) == key(cells[i]):
            j += 1
        out.append((i, "".join(c[0] for c in cells[i:j]), cells[i]))
        i = j
    return out


def render(rows, alt):
    cols = max((len(r) for r in rows), default=0)
    w = int(cols * CELL_W + PAD_X * 2)
    h = int(len(rows) * CELL_H + PAD_Y + 16)

    out = [
        f'<svg xmlns="http://www.w3.org/2000/svg" width="{w}" height="{h}" '
        f'viewBox="0 0 {w} {h}" role="img" aria-label="{html.escape(alt)}">',
        "<style>text{font-family:ui-monospace,SFMono-Regular,'SF Mono',Menlo,Consolas,"
        "monospace;font-size:13px;white-space:pre}.b{font-weight:600}</style>",
        f'<rect width="{w}" height="{h}" rx="10" fill="{BG}"/>',
        f'<rect width="{w}" height="28" rx="10" fill="#161b22"/>',
        f'<rect y="18" width="{w}" height="10" fill="#161b22"/>',
        '<circle cx="20" cy="14" r="5" fill="#ff5f57"/>',
        '<circle cx="36" cy="14" r="5" fill="#febc2e"/>',
        '<circle cx="52" cy="14" r="5" fill="#28c840"/>',
        f'<text x="{w // 2}" y="19" fill="#8b949e" text-anchor="middle" class="b">sbx ui</text>',
    ]

    for n, cells in enumerate(rows):
        y = PAD_Y + n * CELL_H

        # Backgrounds first, as one rect per run, or every cell would be its own element.
        for start, text, cell in runs(cells, lambda c: c[2]):
            if cell[2]:
                out.append(
                    f'<rect x="{PAD_X + start * CELL_W:.1f}" y="{y - 12:.1f}" '
                    f'width="{len(text) * CELL_W:.1f}" height="{CELL_H:.1f}" fill="{cell[2]}"/>'
                )

        spans = []
        for start, text, cell in runs(cells, lambda c: (c[1], c[3])):
            if not text.strip():
                spans.append(html.escape(text))
                continue
            # Nail every glyph to its grid column with an explicit x list. Left to flow, the
            # text advances at the viewer's own monospace metric; if that runs even a little
            # wider than CELL_W, a full-width row drifts off the right of the canvas and off
            # its own background rects - which is exactly what clipped the committed picture.
            # A per-glyph x is honoured by every SVG renderer, unlike textLength, so text,
            # rects and canvas width agree no matter which monospace font the viewer has.
            xs = " ".join(f"{PAD_X + (start + i) * CELL_W:.1f}" for i in range(len(text)))
            cls = ' class="b"' if cell[3] else ""
            spans.append(f'<tspan x="{xs}" fill="{cell[1]}"{cls}>{html.escape(text)}</tspan>')

        if any(c[0].strip() for c in cells):
            out.append(
                f'<text x="{PAD_X}" y="{y:.1f}" xml:space="preserve" fill="{FG}">'
                + "".join(spans)
                + "</text>"
            )

    out.append("</svg>")
    return "\n".join(out)


def main():
    if len(sys.argv) < 3:
        sys.exit("usage: render-ui.py <frame.ansi> <out.svg> [alt]")

    alt = sys.argv[3] if len(sys.argv) > 3 else "The sbx dashboard"

    with open(sys.argv[1], "rb") as f:
        rows = parse(f.read())

    # Trailing blank lines are the frame padding itself out to the terminal's height and are
    # not worth the vertical space in a README.
    while rows and not any(c[0].strip() for c in rows[-1]):
        rows.pop()

    with open(sys.argv[2], "w") as f:
        f.write(render(rows, alt) + "\n")

    print(f"{sys.argv[2]}  {len(rows)} rows")


if __name__ == "__main__":
    main()
