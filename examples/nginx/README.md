# nginx

A web server that costs nothing until somebody loads a page.

```sh
sbx serve --idle 5m &                 # once per machine; nothing answers without it
sbx create my-site --template nginx
eval "$(sbx env my-site --template nginx)"
open "http://$WEB_HOST:$WEB_PORT"
```

Put your own site in it:

```sh
sbx cp my-site nginx ./dist :/usr/share/nginx/html
```

Or give somebody else a link - the tunnel points at the wake port, so the server is asleep
until they open it:

```sh
sbx url my-site nginx
```

This is the smallest useful demonstration of the whole idea: a server with a public URL,
resident memory of **0 B** between visits, and a first request that pays about a second.
