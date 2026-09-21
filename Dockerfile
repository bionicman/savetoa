# syntax=docker/dockerfile:1

FROM ubuntu:26.04

ARG TARGETARCH
ARG GO_VERSION=1.27.1
ARG GO_SHA256_AMD64=63d339f0da5ab53635a56f2490a7984dfe12dfcff22ad749f63edaf590168445
ARG GO_SHA256_ARM64=3450b45a3f9ee8568792736a5c5e70a1f2e9b36c35a8f74958c03e51d7d92bec

ENV PATH="/usr/local/go/bin:${PATH}"

RUN apt-get update \
    && DEBIAN_FRONTEND=noninteractive apt-get install --yes --no-install-recommends \
        ca-certificates \
        curl \
        gcc \
        libc6-dev \
        make \
        sqlite3 \
    && rm -rf /var/lib/apt/lists/*

RUN case "${TARGETARCH}" in \
        amd64) go_sha256="${GO_SHA256_AMD64}" ;; \
        arm64) go_sha256="${GO_SHA256_ARM64}" ;; \
        *) echo "unsupported target architecture: ${TARGETARCH}" >&2; exit 1 ;; \
    esac \
    && curl --fail --location --show-error --silent \
        "https://go.dev/dl/go${GO_VERSION}.linux-${TARGETARCH}.tar.gz" \
        --output /tmp/go.tar.gz \
    && echo "${go_sha256}  /tmp/go.tar.gz" | sha256sum --check - \
    && tar --directory /usr/local --extract --gzip --file /tmp/go.tar.gz \
    && rm /tmp/go.tar.gz

WORKDIR /src

COPY go.* ./
RUN go mod download

COPY Makefile ./
COPY cmd/ ./cmd/
COPY docs/manifest-v1.example.json ./docs/manifest-v1.example.json
COPY internal/ ./internal/
COPY packaging/config.example.yml ./packaging/config.example.yml

CMD ["make", "check"]
