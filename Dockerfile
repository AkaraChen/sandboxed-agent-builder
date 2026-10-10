# sandboxed-agent-builder service image. It needs the Docker socket of the
# daemon that builds (and keeps) the agent images; see deploy/.
FROM golang:1.26-trixie AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/builder ./cmd/builder

FROM debian:trixie-slim
# git fetches plugin repositories and akhook's commit.
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates git \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/builder /usr/local/bin/builder
VOLUME /var/lib/sandboxed-agent-builder
EXPOSE 8080 5000
ENTRYPOINT ["/usr/local/bin/builder"]
CMD ["serve", "-config", "/etc/sandboxed-agent-builder/builder.yaml"]
