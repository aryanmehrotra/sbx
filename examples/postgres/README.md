# postgres

The smallest spec that is still useful.

```sh
sbx serve --idle 5m &                 # once per machine; nothing answers without it
sbx create my-branch
eval "$(sbx env my-branch)"
psql "postgres://app:app@$DATABASE_HOST:$DATABASE_PORT/app" -c '\dt'
```

`init` runs once, after Postgres first reports healthy — so the table is there on a fresh
sandbox and is *not* recreated every time it wakes.

`volume` is what makes sleeping safe: the container is stopped, the data is not.
