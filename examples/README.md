# Examples

Each of these is a `sandbox.json` you can copy into a repo and use as-is.

```sh
cp examples/<one>/sandbox.json .        # if you cloned the repo
sbx init --template <one> > sandbox.json   # if you only have the binary
sbx create my-branch
eval "$(sbx env my-branch)"
```

They are ordinary specs — nothing here is special-cased by sbx.

| | what it gives you | why it is here |
|---|---|---|
| [`nginx`](nginx/) | one nginx | the smallest possible demonstration of the whole idea |
| [`postgres`](postgres/) | Postgres 16 | the smallest useful one |
| [`web-stack`](web-stack/) | Postgres + Redis | the shape most apps actually have |
| [`browser`](browser/) | headless Chrome over CDP | agents that need to look at a page |
| [`analytics`](analytics/) | Postgres + ClickHouse | a heavy service kept `optional` |

## Two things worth copying from all of them

**`health` runs inside the container, so the command has to exist there.** A Chrome image
with no `wget` cannot be health-checked with `wget`, and the failure looks like a service
that never starts. Check with `docker run --rm --entrypoint sh <image> -c 'command -v wget'`.

**`exports` maps onto whatever your code already reads.** Adopting sbx should not mean
editing every script that knows a port number.
