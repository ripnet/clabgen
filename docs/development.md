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
CLABGEN_IMAGE=ghcr.io/ripnet/clabgen:dev containerlab deploy -t b2b.clab.yml
```

`make ui-up` / `make ui-down` run the locally built `clabgen-ui` (from
`deploy/compose.yml`; `CLABGEN_UI_LISTEN=0.0.0.0:8090 make ui-up` to
listen on all interfaces). Re-run `make ui-up` after `make image`. For UI
work, `cd web && npm run dev` serves the UI with hot reload, proxying API
calls to a `clabgen-ui` on :8090.

## Tests

```sh
make test         # go test ./... (add -race as CI does)
make web-test     # Playwright tests of the UI against a mocked agent
cd python && pytest   # the Python package (pip install "./python[test]" first)
```

The Go tests run engines back to back in memory (no root, no
containers). The UI tests use the protocol schemas the Go test
`TestUITypesFixture` writes to `web/e2e/fixtures`; refresh them with
`CLABGEN_WRITE_FIXTURES=1 go test ./internal/server` after changing a
protocol's config or actions (the test fails while they are stale).

The Python package's live tests (`python/tests/test_live.py`) run the
agent binary from `bin/` (`make build`) in an alpine container whose eth1
is a docker network of its own, so they need docker; they're skipped
without it.
`PLAYWRIGHT_CHROMIUM=<path>` makes Playwright use a Chromium of your own.

## Documentation site

`docs/` is also the site at https://clabgen.dev/ (MkDocs with
the Material theme; the README is its home page). Preview it with:

```sh
pip install -r .github/docs/requirements.txt
mkdocs serve        # http://127.0.0.1:8000, rebuilt as you edit
```

CI builds it strictly (a broken link or anchor fails); release tags
publish it.

## Branches, images and releases

- `dev` is where work happens; `main` only receives stable merges.
- Every pull request and push to `main` runs the tests
  ([`ci.yml`](../.github/workflows/ci.yml)). The `internal/core` engine
  tests run in four parallel shards; to run one locally:
  `.github/scripts/test-shard.sh 2 4 -race` (shard 2 of 4).
- A push to `dev` runs them and then publishes `:dev` and `:sha-<commit>`
  images ([`images.yml`](../.github/workflows/images.yml)).
- A tag `vX.Y.Z` on `main` publishes `:X.Y.Z`, `:X.Y` and `:latest`, a
  GitHub release with `clabgenctl` binaries, and the Python package X.Y.Z
  on PyPI (trusted publishing from the `pypi` environment; the version
  comes from the tag). `:latest` only moves on a
  release.

Both images are multi-arch (amd64 and arm64).

## Adding a protocol

See [writing-a-protocol.md](writing-a-protocol.md).
