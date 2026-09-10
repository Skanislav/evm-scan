# syntax=docker/dockerfile:1

# ---------------------------------------------------------------- build
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/evmscand ./cmd/evmscand

# --------------------------------------------------------------- helios
# The light client that stands in for a local geth. It verifies what an upstream
# RPC returns against beacon-chain headers, so the daemon can keep insisting on a
# loopback node without lying to itself about what that node is.
FROM debian:trixie-slim AS helios
ARG TARGETARCH
ARG HELIOS_VERSION=0.11.1
# Pinned per architecture: Railway builds amd64, a laptop `make docker-build` on
# Apple silicon builds arm64. Bump both when bumping the version.
ARG HELIOS_SHA256_AMD64=339bf4ce73073c53790e41e3217b6d91f0e5d8571132b9e88689997613162ddb
ARG HELIOS_SHA256_ARM64=20132e1f772af246eac3885bcba3b54c21a98ac24027a5853eca2fb0edc5dab6
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl && rm -rf /var/lib/apt/lists/*
RUN case "${TARGETARCH}" in \
      amd64) sha="${HELIOS_SHA256_AMD64}" ;; \
      arm64) sha="${HELIOS_SHA256_ARM64}" ;; \
      *) echo "unsupported TARGETARCH ${TARGETARCH}" >&2; exit 1 ;; \
    esac \
 && curl -fsSL -o /tmp/helios.tgz \
      "https://github.com/a16z/helios/releases/download/${HELIOS_VERSION}/helios_linux_${TARGETARCH}.tar.gz" \
 && echo "${sha}  /tmp/helios.tgz" | sha256sum -c - \
 && tar -xzf /tmp/helios.tgz -C /usr/local/bin helios \
 && chmod +x /usr/local/bin/helios

# ---------------------------------------------------------------- final
# Debian rather than a static base: the Helios release binary links glibc, and
# needs 2.39 or newer, which is why this is trixie rather than bookworm.
FROM debian:trixie-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl tini \
 && rm -rf /var/lib/apt/lists/* \
 && useradd --system --create-home --home-dir /data --uid 10001 evmscan
COPY --from=build  /out/evmscand          /app/evmscand
COPY --from=helios /usr/local/bin/helios  /app/helios
COPY web/                                 /app/web/
COPY deploy/config.railway.yaml           /app/config.yaml
# Both hosted profiles ship; EVMSCAN_CONFIG picks one. The Sepolia profile is the
# default because it is the one that can be run without spending real money.
COPY deploy/config.railway.yaml           /app/config.sepolia.yaml
COPY deploy/config.mainnet.yaml           /app/config.mainnet.yaml
COPY deploy/config.mainnet-base.yaml      /app/config.mainnet-base.yaml
COPY deploy/entrypoint.sh                 /app/entrypoint.sh
RUN chmod +x /app/entrypoint.sh && mkdir -p /data/helios && chown -R evmscan:evmscan /data
USER evmscan
WORKDIR /app
EXPOSE 8080
ENTRYPOINT ["/usr/bin/tini", "--", "/app/entrypoint.sh"]
