# ---- Build ----
# Pinned to the platform doing the building, so a foreign target cross-compiles
# instead of running the whole toolchain under emulation
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
WORKDIR /build

# Copy only go.mod and go.sum first to cache dependencies
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

# Copy source code
COPY . .

# Declared here so every layer above stays identical across target platforms
ARG TARGETARCH

# Build with optimizations
# The build cache is what makes a rebuild after a source edit reuse the
# dependencies; keyed per arch, since a cross-compile shares nothing with its host
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build,id=gobuild-$TARGETARCH \
    CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH go build \
    -ldflags="-s -w" \
    -trimpath \
    -o nzbstreamer ./cmd/nzbstreamer

# ---- Release ----
FROM alpine:3.24 AS release
WORKDIR /app

# ca-certificates-bundle is the root store without the openssl tooling that
# ca-certificates drags in; a CGO_ENABLED=0 binary uses crypto/tls, not libssl
#
# fusermount is setuid root, so mounting works unprivileged; /app holds the
# default cache, metadata and watch paths and has to be writable by that user
RUN apk add --no-cache fuse ca-certificates-bundle \
    && adduser -D -u 1000 nzbstreamer \
    && mkdir -p /app/.cache /app/.metadata /app/.watch \
    && chown -R nzbstreamer:nzbstreamer /app

# Copy binary from build stage
COPY --from=build /build/nzbstreamer .

# Configure container
USER 1000:1000
EXPOSE 8080
ENTRYPOINT ["/app/nzbstreamer"]
