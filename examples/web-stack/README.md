# web-stack

Postgres and Redis, as two services that sleep and wake on their own. A branch that only uses
Postgres never starts Redis.

```sh
sbx serve --idle 5m &                 # once per machine; nothing answers without it
sbx create my-branch --template web-stack
eval "$(sbx env my-branch)"           # DATABASE_HOST/PORT and REDIS_HOST/PORT
npm run dev

sbx logs my-branch -f
# postgres | 2026-08-15 ... database system is ready to accept connections
# redis    | 1:M ... Ready to accept connections tcp
```
