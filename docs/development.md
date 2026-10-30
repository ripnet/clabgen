# Development

## Building

```sh
make build        # bin/clabgen-agent, clabgenctl, clabgen-ui (needs Go 1.23+ and Node 20+)
make image        # ghcr.io/ripnet/clabgen:dev and ghcr.io/ripnet/clabgen-ui:dev, locally
```

The web UI (`web/`, React and Vite) is built into `internal/webui/dist`
and embedded in the Go binaries; without it, `go build` still works and
serves a "UI not built" page. `make gen` regenerates the API code from
`api/proto` (needs `protoc` with the Go, Connect and ES plugins).

## Running a local build

The example labs use `ghcr.io/ripnet/clabgen:latest` unless told
otherwise:

```sh
cd examples/b2b
CLABGEN_IMAGE=ghcr.io/ripnet/clabgen:dev clab deploy -t b2b.clab.yml
```

- `make ui-up` / `make ui-down` run the locally built `clabgen-ui` (from
  `deploy/compose.yml`; `CLABGEN_UI_LISTEN=0.0.0.0:8090 make ui-up` to
  listen on all interfaces). Re-run `make ui-up` after `make image`.
- For UI work, `cd web && npm run dev` serves the UI with hot reload,
  proxying API calls to a `clabgen-ui` on :8090.

## Tests

```sh
make test         # go test ./... (add -race as CI does)
make web-test     # Playwright tests of the UI against a mocked agent
cd python && pytest   # the Python package (pip install "./python[test]" first)
```

- The Go tests run engines back to back in memory (no root, no
  containers).
- The UI tests use the protocol schemas the Go test `TestUITypesFixture`
  writes to `web/e2e/fixtures`. Refresh them with
  `CLABGEN_WRITE_FIXTURES=1 go test ./internal/server` after changing a
  protocol's config or actions (the test fails while they are stale).
  `PLAYWRIGHT_CHROMIUM=<path>` makes Playwright use a Chromium of your own.
- The Python package's live tests (`python/tests/test_live.py`) run the
  agent binary from `bin/` (`make build`) in an alpine container whose
  eth1 is a Docker network of its own, so they need Docker; they're
  skipped without it.

## Documentation site

`docs/` is also the site at https://clabgen.dev/ (MkDocs with
the Material theme; the README is its home page). Preview it with:

```sh
make docs           # in Docker, on port 8000 (DOCS_PORT=...); make docs-down stops it
```

or without Docker:

```sh
pip install -r .github/docs/requirements.txt
mkdocs serve        # http://127.0.0.1:8000, rebuilt as you edit
```

CI builds it strictly (a broken link or anchor fails); release tags
publish it.

### Screenshots

Screenshots come in a light and a dark version (`x.png` and
`x-dark.png`), written as a `<picture>` so GitHub shows the one for the
reader's theme; the site's hook turns each into Material's
`#only-light`/`#only-dark` pair, which follows its theme toggle:

```html
<picture>
  <source media="(prefers-color-scheme: dark)" srcset="images/x-dark.png">
  <img alt="What it shows" src="images/x.png">
</picture>
```

`make screenshots` regenerates the README's tour and stills from the
demo lab in `scripts/screenshots` (deployed, driven with Playwright and
destroyed again; see `run.sh` for what it needs). A new screenshot is a
still in `tour.mjs`, copied by `run.sh`, so it is regenerated with the
rest at each release.

### UI and YAML tabs

A config shown both ways is a tab group: GitHub shows the parts one after
the other, and the site's hook turns them into tabs (picking one picks it
on every page):

~~~markdown
<!-- tab: YAML -->
```yaml
devices:
  - {name: v100, port: eth1, vlans: [{vid: 100}], addresses: [10.0.100.1/24]}
```
<!-- tab: UI -->
<picture>…the form-device screenshot…</picture>
<!-- /tabs -->
~~~

The `form-*` screenshots are the demo lab's own configs
(`scripts/screenshots/gen1.yml` and `gen2.yml`), so a tab's YAML is
copied from there: change both together.

## Branches, images and releases

- `dev` is where work happens. `main` only moves forward to a `dev` whose
  CI passed, for a release.
- Every pull request runs the tests
  ([`ci.yml`](../.github/workflows/ci.yml)), as do pushes to `dev` and
  release tags before anything is published. A push to `main` doesn't:
  it's a commit that already passed on `dev`. The `internal/core` engine
  tests run in four parallel shards; to run one locally:
  `.github/scripts/test-shard.sh 2 4 -race` (shard 2 of 4).
- A push to `dev` runs them and then publishes `:dev` and `:sha-<commit>`
  images ([`images.yml`](../.github/workflows/images.yml)).
- A tag `vX.Y.Z` on `main` publishes `:X.Y.Z`, `:X.Y` and `:latest`, a
  GitHub release with `clabgenctl` binaries, and the Python package X.Y.Z
  on PyPI (trusted publishing from the `pypi` environment; the version
  comes from the tag). `:latest` only moves on a release.
- Both images are multi-arch (amd64 and arm64).

## Adding a protocol

See [writing-a-protocol.md](writing-a-protocol.md).
