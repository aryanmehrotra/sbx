# nginx

A web server that costs nothing until somebody loads a page.

```sh
sbx serve --idle 5m &                 # once per machine; nothing answers without it
sbx create my-site --template nginx
eval "$(sbx env my-site)"
open "http://$WEB_HOST:$WEB_PORT"
```

Put your own site in it:

```sh
sbx cp my-site nginx ./dist :/usr/share/nginx/html
```

Or give somebody else a link. The tunnel points at the port sbx holds for the server, so the
server stays asleep until they open it:

```sh
sbx url my-site nginx
```

This is the smallest useful demonstration of the whole idea: a server with a public URL,
**0 B** of RAM between visits, and a first request that waits for the wake (240 ms median, n=20,
v0.14.0, Linux x86_64 cloud VM; [BENCHMARKS.md](../../docs/BENCHMARKS.md#headline-numbers)).
