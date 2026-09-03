# ---- Build ----
FROM golang:1.27-alpine AS build
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
FROM alpine:3.24 AS release
WORKDIR /app

# ca-certificates-bundle is the root store without the openssl tooling that
# ca-certificates drags in; a CGO_ENABLED=0 binary uses crypto/tls, not libssl
RUN apk add --no-cache fuse ca-certificates-bundle

# Copy binary from build stage
COPY --from=build /build/nzbstreamer .

# Configure container
EXPOSE 8080
ENTRYPOINT ["/app/nzbstreamer"]
