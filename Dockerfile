# Build stage
FROM golang:1.25.10-alpine AS builder

WORKDIR /app

# Copy dependency files
COPY go.mod go.sum ./

# Cache Go modules download
RUN --mount=type=cache,target=/go/pkg/mod/ \
    go mod download

# Copy source code
COPY . ./

# Build both API server and CLI binaries with caching enabled
RUN --mount=type=cache,target=/go/pkg/mod/ \
    --mount=type=cache,target=/root/.cache/go-build/ \
    CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o git-updater ./cmd/server && \
    CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o git-updater-cli ./cmd/cli

# Final stage
FROM alpine:3.20

# Install ca-certificates (needed for SSL certificate verification over HTTPS)
RUN apk add --no-cache ca-certificates

# Create a non-root user/group for security
RUN addgroup -S appgroup && adduser -S appuser -G appgroup

WORKDIR /app

# Copy built binaries from builder
COPY --from=builder /app/git-updater .
COPY --from=builder /app/git-updater-cli .

# Create workspace directory and grant ownership to the non-root user
RUN mkdir -p /app/workspace && chown -R appuser:appgroup /app

# Run as non-root user
USER appuser

# Expose port
EXPOSE 3000

# Set environment variables
ENV PORT=3000

# Run the API server binary by default
CMD ["./git-updater"]
