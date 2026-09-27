# browser

A real headless Chrome, asleep until something connects to it.

```sh
sbx serve --idle 5m &                 # once per machine; nothing answers without it
sbx create my-branch --template browser
eval "$(sbx env my-branch)"

curl "http://$CDP_HOST:$CDP_PORT/json/version"
# {"Browser": "HeadlessChrome/124.0.6367.78", ...}
```

Measured: asleep at **0 B**, woken by that request in a median **3.7 s cold** and **0.77 s warm**
(n=5, macOS arm64, v0.1.0 - see [BENCHMARKS.md](../../docs/BENCHMARKS.md#a-heavier-workload-headless-chrome)),
then driven over CDP.
Chrome is a heavy thing to start; the wake is its own startup, not sbx's.

Point Playwright or chromedp at it:

```js
const browser = await chromium.connectOverCDP(`http://${process.env.CDP_HOST}:${process.env.CDP_PORT}`)
```

## Two things that will bite you

**`--remote-debugging-address=0.0.0.0`.** Chrome defaults to binding `[::1]`, which is
unreachable from outside the container. The symptom is a service that starts fine and
answers nothing.

**The health command must exist in the image.** `chromedp/headless-shell` ships no `wget`
and no `curl`, so a `wget` health check can never pass there and the sandbox looks broken.
This example uses `zenika/alpine-chrome`, which has a shell toolchain.
