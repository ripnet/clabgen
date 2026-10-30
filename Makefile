GOBIN ?= $(shell go env GOPATH)/bin
IMAGE ?= ghcr.io/ripnet/clabgen:dev
UI_IMAGE ?= ghcr.io/ripnet/clabgen-ui:dev
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
WKT := Mgoogle/protobuf/struct.proto=google.golang.org/protobuf/types/known/structpb,Mgoogle/protobuf/timestamp.proto=google.golang.org/protobuf/types/known/timestamppb
LDFLAGS := -s -w -X github.com/ripnet/clabgen/internal/version.Version=$(VERSION)

.PHONY: all gen web web-test build test image ui-up ui-down docs docs-down screenshots clean

all: build

gen:
	PATH=$(GOBIN):$$PATH protoc -I api/proto -I /usr/include \
		--go_out=. --go_opt=module=github.com/ripnet/clabgen,$(WKT) \
		--connect-go_out=. --connect-go_opt=module=github.com/ripnet/clabgen,$(WKT) \
		api/proto/clabgen/v1/agent.proto
	protoc -I api/proto -I /usr/include \
		--plugin=protoc-gen-es=web/node_modules/.bin/protoc-gen-es \
		--es_out=web/src/gen --es_opt=target=ts \
		api/proto/clabgen/v1/agent.proto

# The UI is embedded into the Go binaries from internal/webui/dist.
web:
	rm -rf internal/webui/dist/assets internal/webui/dist/index.html
	cd web && npm ci --no-fund --no-audit && npm run build

# UI tests (Playwright, agent API mocked). PLAYWRIGHT_CHROMIUM=<path> uses
# a Chromium other than Playwright's own (npx playwright install chromium).
web-test:
	cd web && npm run e2e

build: web
	CGO_ENABLED=0 go build -ldflags '$(LDFLAGS)' -o bin/ ./cmd/...

test:
	go test ./...

image:
	docker build --build-arg VERSION=$(VERSION) --target agent -t $(IMAGE) .
	docker build --build-arg VERSION=$(VERSION) --target ui -t $(UI_IMAGE) .

# The per-host UI container (deploy/compose.yml). Re-run ui-up after
# `make image` to pick up a new build.
ui-up:
	docker compose -f deploy/compose.yml up -d --force-recreate

ui-down:
	docker compose -f deploy/compose.yml down

# A live preview of the documentation site on DOCS_PORT, rebuilt as docs/,
# README.md or mkdocs.yml change (Material's image, at CI's version).
DOCS_PORT ?= 8000
MKDOCS_MATERIAL := $(shell sed -n 's/^mkdocs-material==//p' .github/docs/requirements.txt)
docs:
	-docker rm -f clabgen-docs >/dev/null 2>&1
	docker run -d --name clabgen-docs -p $(DOCS_PORT):8000 -v $(CURDIR):/docs \
		squidfunk/mkdocs-material:$(MKDOCS_MATERIAL) serve -a 0.0.0.0:8000 --livereload -w README.md
	@echo "docs: http://$$(hostname):$(DOCS_PORT)/ (warnings: docker logs clabgen-docs; stop: make docs-down)"

docs-down:
	docker rm -f clabgen-docs

# The README tour and stills, light and dark, from the demo lab in
# scripts/screenshots (deployed and destroyed; see run.sh).
screenshots:
	scripts/screenshots/run.sh

clean:
	rm -rf bin
