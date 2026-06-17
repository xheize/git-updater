# Build stage
FROM golang:1.25-alpine AS builder

WORKDIR /app

# Copy dependency files
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY main.go ./

# Build the binary
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o git-updater main.go

# Final stage
FROM alpine:3.20

# Install git and openssh-client (needed for clone/push operations)
RUN apk add --no-cache git openssh-client

WORKDIR /app

# Copy built binary from builder
COPY --from=builder /app/git-updater .

# Expose port
EXPOSE 3000

# Set environment variables
ENV PORT=3000

# Run the binary
CMD ["./git-updater"]
