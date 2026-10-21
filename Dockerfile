FROM node:20-alpine AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-fund --no-audit
COPY web/ ./
RUN mkdir -p ../internal/webui/dist && npm run build

FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /src/internal/webui/dist/ ./internal/webui/dist/
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X github.com/ripnet/clabgen/internal/version.Version=${VERSION}" \
      -o /out/ ./cmd/clabgen-agent ./cmd/clabgenctl ./cmd/clabgen-ui

FROM alpine:3.20
RUN apk add --no-cache iproute2 tcpdump ethtool
COPY --from=build /out/ /usr/local/bin/
ENV CLABGEN_SERVER=localhost:8080
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/clabgen-agent"]
