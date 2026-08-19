# pr-preview

A URL per pull request, for reviewers - on a box you own, where the idle ones cost **0 B** of
memory instead of a per-environment bill.

This is the use case the comparison sends to Northflank, Uffizzi and Okteto. What they give you
that sbx does not is a managed control plane and a team UI; what sbx gives you that they do not
is that a preview nobody is looking at sleeps to zero and wakes when a reviewer opens the link.
On a hosted preview platform an idle environment still costs; here it does not.

## The shape

sbx runs on one persistent host you own - a small always-on VM, not the CI runner - with
`sbx serve` up and a **golden** sandbox seeded once (schema, fixtures, migrations):

```sh
# on the preview host, once
sbx serve --idle 30m &
sbx create golden --template postgres
sbx exec golden postgres psql -U app -d app -f schema.sql   # seed + migrate once
sbx snapshot golden golden
```

Then CI, per pull request:

- **opened / updated** → `sbx fork golden pr-<number>` gives that PR its own copy of the data
  (a write in one PR is invisible to the others), the app build is deployed into it, and
  `sbx url pr-<number> web` prints a public link that **wakes the env when a reviewer opens it**.
  The link goes on the PR as a comment.
- **closed** → `sbx rm pr-<number>` destroys the sandbox and its volume.

Between reviews the environment sleeps to 0 B and the next click wakes it - the reviewer waits
the one wake (sub-second for most stacks), and you pay for nothing in between.

## The workflow

[`pr-preview.yml`](pr-preview.yml) is a GitHub Actions template. It SSHes to the preview host
and drives sbx there; fill in the two secrets it names (`PREVIEW_HOST`, `PREVIEW_SSH_KEY`) and
adapt the deploy step to how your app is built. It is a starting point, not a turnkey platform -
the teardown-on-close job is the part worth copying exactly, because a preview env that outlives
its PR is the leak these platforms exist to prevent.

## Why fork, not create

`sbx fork golden pr-N` starts each PR from the **seeded** state, so a preview is ready the moment
it wakes instead of running migrations on first open. The fork is copy-per-PR: twenty open PRs
are twenty independent databases, and closing one takes its data with it. `sbx gc --snapshots`
on the host reclaims anything a missed teardown left behind.
