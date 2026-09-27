# browser

A headless Chrome, asleep until something connects. Playwright, Puppeteer and chromedp drive it
over CDP (the Chrome DevTools Protocol).

```sh
sbx serve --idle 5m &                 # once per machine; nothing answers without it
sbx create my-branch --template browser
eval "$(sbx env my-branch)"

curl "http://$CDP_HOST:$CDP_PORT/json/version"
# {"Browser": "HeadlessChrome/124.0.6367.78", ...}
```

```js
const browser = await chromium.connectOverCDP(`http://${process.env.CDP_HOST}:${process.env.CDP_PORT}`)
```

It sleeps at 0 B of RAM. The first wake in a session takes seconds (3.7 s median, macOS arm64,
v0.1.0); once the image is warm it is well under a second (387 ms median, n=5, v0.14.0, Linux
x86_64 cloud VM). Most of that is Chrome's own startup
([BENCHMARKS.md](../../docs/BENCHMARKS.md#a-heavier-workload-headless-chrome)).

## If you write your own spec

- Pass `--remote-debugging-address=0.0.0.0`. Chrome binds `[::1]` by default, which is unreachable
  from outside the container, so the service starts and answers nothing.
- The health command must exist in the image. `chromedp/headless-shell` has no `wget` or `curl`,
  so a `wget` check never passes. This example uses `zenika/alpine-chrome`, which has both.
