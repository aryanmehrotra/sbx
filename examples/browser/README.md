# browser

A real headless Chrome, asleep until something connects to it. Playwright, Puppeteer and
chromedp drive it over CDP (the Chrome DevTools Protocol).

```sh
sbx serve --idle 5m &                 # once per machine; nothing answers without it
sbx create my-branch --template browser
eval "$(sbx env my-branch)"

curl "http://$CDP_HOST:$CDP_PORT/json/version"
# {"Browser": "HeadlessChrome/124.0.6367.78", ...}
```

Measured: asleep at **0 B** of RAM. Woken by that request, plan for seconds on the first touch
in a session (3.7 s median, macOS arm64, v0.1.0) and well under a second once the image is warm
(387 ms median, n=5, v0.14.0, Linux x86_64 cloud VM). Details:
[BENCHMARKS.md](../../docs/BENCHMARKS.md#a-heavier-workload-headless-chrome). Most of the wake is
Chrome's own startup, not sbx's.

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
