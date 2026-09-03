# ---- Build ----
# Pinned to the platform doing the building, so a foreign target cross-compiles
# instead of running the whole toolchain under emulation
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
WORKDIR /build

# Copy only go.mod and go.sum first to cache dependencies
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Declared here so every layer above stays identical across target platforms
ARG TARGETARCH

# Build with optimizations
RUN CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH go build \
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
