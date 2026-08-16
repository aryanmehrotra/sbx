#!/usr/bin/env python3
"""Render captured terminal lines as an animated SVG.

    render-demo.py <lines-file> <out.svg>

The input is one `kind<TAB>text` per line, written by scripts/demo.sh from real command
output. Kinds: cmd, out, ok, info, dim, blank.

Self-contained by construction: no external libraries, no fonts to fetch, no scripts, and
no reveal animation - GitHub strips <script> from rendered SVG and blocks remote references,
and anything that fades in renders blank in a static snapshot. The type is whatever monospace
the reader already has.
"""

import html
import sys

# Terminal palette. Chosen to stay legible on GitHub's light and dark page backgrounds, which
# is why the terminal paints its own dark ground rather than inheriting one.
BG, CHROME, FG = "#0d1117", "#161b22", "#c9d1d9"
GREEN, CYAN, DIM, BLUE = "#3fb950", "#39c5cf", "#8b949e", "#58a6ff"

LINE_H = 20.5
PAD_X = 22
TOP = 74
FONT = 13.5


def colour(kind: str) -> str:
    return {
        "cmd": FG,
        "ok": GREEN,
        "info": CYAN,
        "dim": DIM,
    }.get(kind, FG)


def main() -> int:
    if len(sys.argv) != 3:
        print(__doc__, file=sys.stderr)
        return 2

    rows = []
    with open(sys.argv[1], encoding="utf-8") as f:
        for raw in f:
            raw = raw.rstrip("\n")
            if not raw:
                continue
            kind, _, text = raw.partition("\t")
            rows.append((kind.strip().lower(), text))

    if not rows:
        print("render-demo: nothing captured", file=sys.stderr)
        return 1

    height = int(TOP + LINE_H * (len(rows) + 1) + 24)
    width = 900

    # No reveal animation. Every line is simply visible.
    #
    # Three versions of this file tried to type the lines out one at a time, and all three
    # rendered a blank terminal somewhere that matters. The last one was the instructive
    # failure: the hidden state was correct CSS, sitting only in the keyframes, and it still
    # came out empty - because a thumbnailer snapshots the animation at t=0, and at t=0 an
    # unrevealed line is *supposed* to be transparent. There is no way to write a reveal that
    # a static snapshot survives, because the first frame of a reveal is by definition empty.
    #
    # That trade is a bad one for a README hero. It buys a typing effect on GitHub's rendered
    # page and pays with an empty rectangle in the social preview card, in PDF export, in
    # editor previews and in every screenshot anyone takes of it. The content is the point;
    # the motion was decoration.
    #
    # The cursor still blinks: at t=0 it is opaque, so it costs nothing in a snapshot.
    css = [
        "text{font-family:ui-monospace,SFMono-Regular,'SF Mono',Menlo,Consolas,monospace;"
        f"font-size:{FONT}px;white-space:pre}}",
        ".b{font-weight:600}",
        "@media (prefers-reduced-motion:no-preference){"
        ".cur{animation:blink 1s steps(1) infinite}"
        "@keyframes blink{0%,50%{opacity:1}50.01%,100%{opacity:0}}"
        "}",
    ]

    alt = (
        "A terminal running sbx: a sandbox is created from a template, its addresses are "
        "exported, an agent reads them as JSON, a service is added mid-task, a snapshot is "
        "forked, the sandbox sleeps to zero and a plain connection wakes it."
    )

    out = [
        f'<svg xmlns="http://www.w3.org/2000/svg" width="{width}" height="{height}" '
        f'viewBox="0 0 {width} {height}" role="img" aria-label="{html.escape(alt)}">',
        "<style>" + "".join(css) + "</style>",
        f'<rect width="{width}" height="{height}" rx="10" fill="{BG}"/>',
        f'<rect width="{width}" height="46" rx="10" fill="{CHROME}"/>',
        f'<rect y="36" width="{width}" height="10" fill="{CHROME}"/>',
        '<circle cx="26" cy="23" r="6" fill="#ff5f57"/>',
        '<circle cx="46" cy="23" r="6" fill="#febc2e"/>',
        '<circle cx="66" cy="23" r="6" fill="#28c840"/>',
        f'<text x="{width // 2}" y="28" fill="{DIM}" text-anchor="middle" class="b">sbx</text>',
    ]

    y = TOP
    for kind, text in rows:
        if kind == "blank":
            y += LINE_H
            continue

        esc = html.escape(text)

        if kind == "cmd" and text.lstrip().startswith("#"):
            out.append(
                f'<text x="{PAD_X}" y="{y:.1f}" xml:space="preserve" fill="{DIM}">{esc}</text>'
            )
        elif kind == "cmd":
            out.append(
                f'<text x="{PAD_X}" y="{y:.1f}" xml:space="preserve">'
                f'<tspan fill="{BLUE}" class="b">$ </tspan>'
                f'<tspan fill="{FG}" class="b">{esc}</tspan></text>'
            )
        elif kind == "info":
            label, _, rest = text.partition("\t")
            out.append(
                f'<text x="{PAD_X}" y="{y:.1f}" xml:space="preserve">'
                f'<tspan fill="{CYAN}" class="b">INFO </tspan>'
                f'<tspan fill="{DIM}">{html.escape(label)}</tspan>'
                f'<tspan fill="{FG}">  {html.escape(rest)}</tspan></text>'
            )
        else:
            out.append(
                f'<text x="{PAD_X}" y="{y:.1f}" xml:space="preserve" '
                f'fill="{colour(kind)}">{esc}</text>'
            )

        y += LINE_H

    out.append(
        f'<rect x="{PAD_X}" y="{y - 12:.1f}" width="8" height="15" fill="{FG}" class="cur"/>'
    )
    out.append("</svg>")

    with open(sys.argv[2], "w", encoding="utf-8") as f:
        f.write("\n".join(out) + "\n")

    print(f"render-demo: {len(rows)} lines, {height}px", file=sys.stderr)

    return 0


if __name__ == "__main__":
    raise SystemExit(main())
