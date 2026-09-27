# Build stage
FROM golang:1.25.10-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod/ go mod download
COPY . ./
RUN --mount=type=cache,target=/go/pkg/mod/ \
    --mount=type=cache,target=/root/.cache/go-build/ \
    CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o git-updater ./cmd/server && \
    CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o git-updater-cli ./cmd/cli

# Shared non-root runtime; set directory ownership before copying binaries.
FROM alpine:3.20 AS runtime
RUN apk add --no-cache ca-certificates && \
    addgroup -S -g 101 appgroup && \
    adduser -S -u 100 -G appgroup appuser && \
    mkdir -p /app && chown 100:101 /app
WORKDIR /app
USER 100:101

# One-shot API client: docker build --target cli ...
FROM runtime AS cli
COPY --from=builder --chown=100:101 /app/git-updater-cli /app/git-updater-cli
ENTRYPOINT ["/app/git-updater-cli"]
CMD ["help"]

# Keep the server last so existing builds still produce the server image.
FROM runtime AS server
RUN mkdir -p /app/workspace /app/data
COPY --from=builder --chown=100:101 /app/git-updater /app/git-updater
EXPOSE 3000
VOLUME ["/app/data"]
ENV PORT=3000
CMD ["./git-updater"]
