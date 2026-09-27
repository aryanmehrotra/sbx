# nginx

A web server that uses no memory until somebody loads a page.

```sh
sbx serve --idle 5m &                 # once per machine; nothing answers without it
sbx create my-site --template nginx
eval "$(sbx env my-site)"
open "http://$WEB_HOST:$WEB_PORT"

sbx cp my-site nginx ./dist :/usr/share/nginx/html   # your own site
sbx url my-site nginx                                # a public link; the server sleeps until it is opened
```

It sleeps at 0 B of RAM between visits. The first request waits for the wake (240 ms median,
n=20, v0.14.0, Linux x86_64 cloud VM; [BENCHMARKS.md](../../docs/BENCHMARKS.md#headline-numbers)).
