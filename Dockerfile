# ---- Build ----
FROM golang:1.23-alpine AS build
WORKDIR /build

# Install build dependencies
RUN apk add --no-cache ca-certificates

# Copy only go.mod and go.sum first to cache dependencies
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build with optimizations
RUN CGO_ENABLED=0 go build \
    -ldflags="-s -w" \
    -trimpath \
    -o nzbstreamer ./cmd/nzbstreamer

# ---- Release ----
FROM alpine:3.19 AS release
WORKDIR /app

# Install runtime dependencies
RUN apk add --no-cache fuse ca-certificates

# Copy binary from build stage
COPY --from=build /build/nzbstreamer .

# Create non-root user
RUN adduser -D appuser && \
    chown -R appuser:appuser /app
USER appuser

# Configure container
EXPOSE 8080
ENTRYPOINT ["/app/nzbstreamer"]
