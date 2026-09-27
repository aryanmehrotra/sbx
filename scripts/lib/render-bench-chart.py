#!/usr/bin/env python3
"""Draw the README's benchmark chart: docs/bench-light.svg and docs/bench-dark.svg.

    python3 scripts/lib/render-bench-chart.py

The numbers are typed in below, not measured here: each one is copied from docs/BENCHMARKS.md,
which names the script and the machine. Change them there first, then here. Two files because
GitHub picks one per theme through <picture>; a single file would need a background of its own
and would read as a pasted screenshot on the other theme.
"""
import os

ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

PANELS = [
    {
        "title": "OpenSandbox API: create a sandbox and run a first command",
        "unit": "ms",
        "rows": [
            ("sbx", 307, "307 ms", "same SDK, same image"),
            ("OpenSandbox server", 1417, "1,417 ms", ""),
        ],
    },
    {
        "title": "Memory held by 20 idle Postgres databases",
        "unit": "MB",
        "rows": [
            ("sbx, asleep", 17.6, "17.6 MB", ""),
            ("docker compose, always on", 629.4, "629 MB", ""),
        ],
    },
    {
        "title": "Time until a sleeping Postgres answers psql",
        "unit": "ms",
        "rows": [
            ("sbx", 348, "348 ms", "first try served: 20 of 20"),
            ("Lazytainer (wakes on traffic)", 3407, "3,407 ms", "first try refused: 0 of 5"),
        ],
    },
]

CAPTION = ("Lower is better. sbx v0.14.0 on a 4 vCPU Linux VM with Docker 29. Memory: lower of 2 runs; "
           "times: medians. Method in docs/BENCHMARKS.md.")

THEMES = {
    "light": {"text": "#1f2328", "muted": "#59636e", "sbx": "#1a7f37", "other": "#6e7781", "rule": "#d1d9e0"},
    "dark": {"text": "#e6edf3", "muted": "#9198a1", "sbx": "#2ea043", "other": "#656d76", "rule": "#3d444d"},
}

W, LABEL_W, BAR_X, BAR_MAX, ROW_H, BAR_H = 900, 210, 220, 470, 34, 18
FONT = "-apple-system,BlinkMacSystemFont,'Segoe UI',Helvetica,Arial,sans-serif"


def esc(s):
    return s.replace("&", "&amp;").replace("<", "&lt;")


def render(theme):
    c = THEMES[theme]
    out, y = [], 8
    for p in PANELS:
        out.append(f'<text x="0" y="{y + 14}" font-size="15" font-weight="600" fill="{c["text"]}">{esc(p["title"])}</text>')
        y += 30
        top = max(v for _, v, _, _ in p["rows"])
        for name, v, label, note in p["rows"]:
            is_sbx = name.startswith("sbx")
            bold = ' font-weight="600"' if is_sbx else ""
            w = max(3, BAR_MAX * v / top)
            cy = y + ROW_H / 2
            out.append(f'<text x="{LABEL_W}" y="{cy + 5}" font-size="13" text-anchor="end" '
                       f'fill="{c["text"]}"{bold}>{esc(name)}</text>')
            out.append(f'<rect x="{BAR_X}" y="{cy - BAR_H / 2}" width="{w:.1f}" height="{BAR_H}" rx="3" '
                       f'fill="{c["sbx"] if is_sbx else c["other"]}"/>')
            tx = BAR_X + w + 8
            out.append(f'<text x="{tx:.1f}" y="{cy + 5}" font-size="13" fill="{c["text"]}"'
                       f'{bold}>{esc(label)}'
                       + (f'<tspan fill="{c["muted"]}" font-weight="400">  ·  {esc(note)}</tspan>' if note else "")
                       + "</text>")
            y += ROW_H
        y += 16
    out.append(f'<line x1="0" x2="{W}" y1="{y}" y2="{y}" stroke="{c["rule"]}" stroke-width="1"/>')
    y += 20
    out.append(f'<text x="0" y="{y}" font-size="12" fill="{c["muted"]}">{esc(CAPTION)}</text>')
    h = y + 8
    alt = "; ".join(f'{p["title"]}: ' + ", ".join(f"{n} {l}" for n, _, l, _ in p["rows"]) for p in PANELS)
    return (f'<svg xmlns="http://www.w3.org/2000/svg" width="{W}" height="{h}" viewBox="0 0 {W} {h}" '
            f'role="img" aria-label="{esc(alt)}" font-family="{FONT}">\n' + "\n".join(out) + "\n</svg>\n")


for t in THEMES:
    with open(os.path.join(ROOT, "docs", f"bench-{t}.svg"), "w") as f:
        f.write(render(t))
print("wrote docs/bench-light.svg docs/bench-dark.svg")
