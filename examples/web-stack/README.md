# web-stack

Postgres and Redis — the shape most applications actually have.

```sh
sbx serve --idle 5m &                 # once per machine; nothing answers without it
sbx create my-branch
eval "$(sbx env my-branch)"
# DATABASE_HOST/PORT and REDIS_HOST/PORT are now set
npm run dev
```

The two services **sleep and wake independently**. A branch that only ever touches Postgres
never pays for Redis, and vice versa — which is why this is two services rather than one
image with both in it.

Watch them as one thing:

```sh
sbx logs my-branch -f
# postgres | 2026-08-15 ... database system is ready to accept connections
# redis    | 1:M ... Ready to accept connections tcp
```
