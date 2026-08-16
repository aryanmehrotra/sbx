#!/usr/bin/env python3
"""Render captured terminal lines as an animated SVG.

    render-demo.py <lines-file> <out.svg>

The input is one `kind<TAB>text` per line, written by scripts/demo.sh from real command
output. Kinds: cmd, out, ok, info, dim, blank.

Self-contained by construction: no external libraries, no fonts to fetch, no scripts. GitHub
strips <script> from rendered SVG and blocks remote references, so the typing effect is CSS
keyframes and the type is whatever monospace the reader already has.
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

    # Each line appears in turn, then the whole thing loops. A command line dwells a little
    # longer than its output, which is roughly how a person reads it.
    steps, t = [], 0.0
    for kind, _ in rows:
        steps.append(t)
        t += 0.55 if kind == "cmd" else 0.30
    total = round(t + 2.4, 2)

    # Everything is visible by default, and the typing effect is layered on top only where
    # motion is welcome.
    #
    # The obvious way round - start every line at opacity:0 and animate it in - renders a
    # completely empty terminal anywhere the animation does not run: a static rasteriser, a
    # reader with prefers-reduced-motion, a markdown viewer that strips CSS. The previous
    # demo had exactly that bug, and it was invisible until someone rasterised it.
    css = [
        "text{font-family:ui-monospace,SFMono-Regular,'SF Mono',Menlo,Consolas,monospace;"
        f"font-size:{FONT}px;white-space:pre}}",
        ".b{font-weight:600}",
        "@media (prefers-reduced-motion:no-preference){",
    ]
    for i, at in enumerate(steps):
        pct = round(100.0 * at / total, 2)
        css.append(
            f".l{i}{{opacity:0;animation:k{i} {total}s steps(1) infinite}}"
            f"@keyframes k{i}{{0%,{pct}%{{opacity:0}}{pct}%,100%{{opacity:1}}}}"
        )
    css.append(
        ".cur{animation:blink 1s steps(1) infinite}"
        "@keyframes blink{0%,50%{opacity:1}50.01%,100%{opacity:0}}"
    )
    css.append("}")

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
    for i, (kind, text) in enumerate(rows):
        if kind == "blank":
            y += LINE_H
            continue

        esc = html.escape(text)

        if kind == "cmd" and text.lstrip().startswith("#"):
            out.append(
                f'<text x="{PAD_X}" y="{y:.1f}" xml:space="preserve" fill="{DIM}" '
                f'class="l{i}">{esc}</text>'
            )
        elif kind == "cmd":
            out.append(
                f'<text x="{PAD_X}" y="{y:.1f}" xml:space="preserve" class="l{i}">'
                f'<tspan fill="{BLUE}" class="b">$ </tspan>'
                f'<tspan fill="{FG}" class="b">{esc}</tspan></text>'
            )
        elif kind == "info":
            label, _, rest = text.partition("\t")
            out.append(
                f'<text x="{PAD_X}" y="{y:.1f}" xml:space="preserve" class="l{i}">'
                f'<tspan fill="{CYAN}" class="b">INFO </tspan>'
                f'<tspan fill="{DIM}">{html.escape(label)}</tspan>'
                f'<tspan fill="{FG}">  {html.escape(rest)}</tspan></text>'
            )
        else:
            out.append(
                f'<text x="{PAD_X}" y="{y:.1f}" xml:space="preserve" fill="{colour(kind)}" class="l{i}">{esc}</text>'
            )

        y += LINE_H

    out.append(f'<rect x="{PAD_X}" y="{y - 12:.1f}" width="8" height="15" fill="{FG}" class="cur"/>')
    out.append("</svg>")

    with open(sys.argv[2], "w", encoding="utf-8") as f:
        f.write("\n".join(out) + "\n")

    print(f"render-demo: {len(rows)} lines, {height}px", file=sys.stderr)

    return 0


if __name__ == "__main__":
    raise SystemExit(main())
