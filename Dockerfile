# syntax=docker/dockerfile:1

FROM ubuntu:26.04

RUN apt-get update \
    && DEBIAN_FRONTEND=noninteractive apt-get install --yes --no-install-recommends \
        ca-certificates \
        gcc \
        golang-go \
        libc6-dev \
        make \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /src

COPY go.* ./
RUN go mod download

COPY Makefile ./
COPY cmd/ ./cmd/
COPY docs/manifest-v1.example.json ./docs/manifest-v1.example.json
COPY internal/ ./internal/
COPY packaging/config.example.yml ./packaging/config.example.yml

CMD ["make", "check"]
