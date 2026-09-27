# pr-preview

A preview URL per pull request, on a machine you own. Idle previews use 0 B of RAM. Hosted
platforms such as Northflank, Uffizzi and Okteto add a managed control plane and a team UI
([COMPARISON.md](../../docs/COMPARISON.md)).

## Setup

Run sbx on one persistent host, a small always-on VM rather than the CI runner. Seed a golden
sandbox once and snapshot it:

```sh
sbx serve --idle 30m &
sbx create golden --template postgres
sbx cp   golden postgres ./schema.sql :/tmp/schema.sql
sbx exec golden postgres psql -U app -d app -f /tmp/schema.sql   # seed and migrate once
sbx snapshot golden golden
```

Then CI, per pull request:

- Opened or updated: `sbx fork golden pr-<number>` gives the PR its own copy of the data. CI
  deploys the app build into it and posts `sbx url pr-<number> web` as a PR comment.
- Closed: `sbx rm pr-<number>` removes the sandbox and its volume.

A reviewer's click waits for one wake (348 ms median for postgres on v0.14.0,
[BENCHMARKS.md](../../docs/BENCHMARKS.md#sbx-by-itself)). Each PR starts from the seeded state,
so there are no migrations on first open. `sbx gc --snapshots` reclaims anything a missed
teardown left.

## The workflow

[`pr-preview.yml`](pr-preview.yml) is a GitHub Actions template that SSHes to the preview host.
Set its two secrets (`PREVIEW_HOST`, `PREVIEW_SSH_KEY`) and adapt the deploy step to your app.
Copy the teardown-on-close job exactly: a preview that outlives its PR is a leak.
