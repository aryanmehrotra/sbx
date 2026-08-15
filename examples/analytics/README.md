# analytics

Postgres for the app, ClickHouse for the analytics — and ClickHouse marked `optional`.

```sh
sbx create my-branch              # postgres only
sbx create my-branch --optional   # both
```

**Why optional.** An idle ClickHouse is about 200 MB, against Postgres's 22 MB, and most
branches never query it. An optional service is not created unless you ask, but it **still
reserves its ports** — so adding it later never moves Postgres out from under a config that
recorded where it was.

`files` mounts a config that bounds ClickHouse's caches. Measured honestly: on a fresh idle
server it saves nothing — tuned and untuned are within 2 MB. It matters under load, where
the mark cache would otherwise grow toward its 5 GiB default.

⚠️ Two settings are deliberately absent from that config: `max_thread_pool_size` and
`background_pool_size`. ClickHouse 24.3 exits during startup when either is set, silently,
and the container looks like a service that never came up.
