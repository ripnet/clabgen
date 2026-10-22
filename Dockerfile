# The UI and the Go binaries are built on the build platform and
# cross-compiled, so multi-arch images need no emulated compilers.
FROM --platform=$BUILDPLATFORM node:20-alpine AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-fund --no-audit
COPY web/ ./
RUN mkdir -p ../internal/webui/dist && npm run build

FROM --platform=$BUILDPLATFORM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /src/internal/webui/dist/ ./internal/webui/dist/
ARG VERSION=dev
ARG TARGETOS=linux
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
      -ldflags "-s -w -X github.com/ripnet/clabgen/internal/version.Version=${VERSION}" \
      -o /out/ ./cmd/clabgen-agent ./cmd/clabgenctl ./cmd/clabgen-ui

# ghcr.io/ripnet/clabgen-ui (docker build --target ui): the lab-wide web
# UI, finding agents through the Docker socket.
FROM alpine:3.20 AS ui
COPY --from=build /out/clabgen-ui /usr/local/bin/
EXPOSE 8090
ENTRYPOINT ["/usr/local/bin/clabgen-ui"]

# ghcr.io/ripnet/clabgen (the default target): the agent, with the CLI and
# clabgen-ui.
FROM alpine:3.20 AS agent
RUN apk add --no-cache iproute2 tcpdump ethtool
COPY --from=build /out/ /usr/local/bin/
ENV CLABGEN_SERVER=localhost:8080
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/clabgen-agent"]
