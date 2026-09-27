# postgres

The smallest useful spec.

```sh
sbx serve --idle 5m &                 # once per machine; nothing answers without it
sbx create my-branch --template postgres
eval "$(sbx env my-branch)"
psql "postgres://app:app@$DATABASE_HOST:$DATABASE_PORT/app" -c '\dt'
```

- `init` runs once, after Postgres first reports healthy. The table exists on a fresh sandbox and
  is not recreated on each wake.
- `volume` keeps the data while the container is stopped.
