# Builder image for shelly-fritz-proxy.
#
# A single image cross-compiles the Go binary for every supported target
# (amd64, arm64, armv7) and packages it as both .deb and .rpm via nfpm.
# Unlike fritzhome-cache's C++ build, no distro-matched glibc or OpenSSL
# is needed: Go statically links its standard library (CGO_ENABLED=0)
# and produces a self-contained ELF that runs on glibc-based and
# musl-based distros alike.
#
# Tags as `shelly-fritz-proxy-builder`.

FROM golang:1.26-bookworm

LABEL org.opencontainers.image.title="shelly-fritz-proxy-builder"
LABEL org.opencontainers.image.description="Cross-compile + package shelly-fritz-proxy for all supported Linux distros."

ENV DEBIAN_FRONTEND=noninteractive
ENV GOTOOLCHAIN=local

# nfpm version. Bumping this is safe: the manifest schema is stable
# within the 2.x series.
ARG NFPM_VERSION=2.39.0

RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates \
        curl \
        gzip \
        tar \
        xz-utils \
        file \
    && rm -rf /var/lib/apt/lists/*

# Install nfpm. Hash-pinning is intentionally omitted to keep the
# Dockerfile readable; if you want airtight supply chain control,
# vendor the binary into your fork and COPY it in instead of curl-ing.
RUN set -eux; \
    arch="$(uname -m)"; \
    case "$arch" in \
      x86_64)  nfpm_arch="x86_64" ;; \
      aarch64) nfpm_arch="arm64" ;; \
      *) echo "unsupported host arch: $arch" >&2; exit 1 ;; \
    esac; \
    url="https://github.com/goreleaser/nfpm/releases/download/v${NFPM_VERSION}/nfpm_${NFPM_VERSION}_Linux_${nfpm_arch}.tar.gz"; \
    curl -fsSL "$url" -o /tmp/nfpm.tar.gz; \
    tar -xzf /tmp/nfpm.tar.gz -C /usr/local/bin nfpm; \
    rm -f /tmp/nfpm.tar.gz; \
    nfpm --version

# Ensure the build directory exists and is writable by any uid we run as.
RUN mkdir -p /build && chmod 1777 /build

# Mountpoint for the source tree (read-only).
WORKDIR /src
