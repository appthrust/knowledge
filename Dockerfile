# syntax=docker/dockerfile:1

# SPA output is architecture-independent; build natively without QEMU.
FROM --platform=$BUILDPLATFORM oven/bun:1.4.1-alpine AS spa-builder
WORKDIR /workspace/web
COPY web/ ./
RUN --mount=type=secret,id=NODE_AUTH_TOKEN,required=true \
    NODE_AUTH_TOKEN="$(cat /run/secrets/NODE_AUTH_TOKEN)" bun install --frozen-lockfile
RUN bun run build:image

FROM --platform=$BUILDPLATFORM docker.io/golang:1.27.3 AS server-builder
WORKDIR /workspace
ARG TARGETOS
ARG TARGETARCH
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd/server ./cmd/server
RUN --mount=type=cache,target=/root/.cache/go-build --mount=type=cache,target=/go/pkg/mod \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags='-s -w' -o /out/knowledge-server ./cmd/server

# No JavaScript runtime is needed: the Go server serves the SPA and the API.
FROM gcr.io/distroless/static:nonroot
COPY --from=server-builder /out/knowledge-server /knowledge-server
COPY --from=spa-builder /workspace/web/dist /srv/knowledge
USER 65532
EXPOSE 8080
ENTRYPOINT ["/knowledge-server"]
