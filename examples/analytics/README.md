# analytics

Postgres for the app, and ClickHouse marked `optional`.

```sh
sbx serve --idle 5m &                 # once per machine; nothing answers without it
sbx create my-branch --template analytics              # postgres only
sbx create my-branch --template analytics --optional   # both
```

An idle ClickHouse holds about 200 MB of RAM (measured at v0.1.0, see
[BENCHMARKS.md](../../docs/BENCHMARKS.md#head-to-head)), and most branches never query it. An optional
service is not created unless you ask. It still reserves its ports, so adding it later never moves
Postgres.

`files` mounts a config that bounds ClickHouse's caches. On a fresh idle server it saves nothing
(tuned and untuned are within 2 MB). Under load it stops the mark cache growing toward its 5 GiB
default.

The config leaves out `max_thread_pool_size` and `background_pool_size` on purpose. ClickHouse
24.3 exits silently during startup when either is set, and looks like a service that never came up.
