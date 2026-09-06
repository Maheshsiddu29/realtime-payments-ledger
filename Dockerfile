# syntax=docker/dockerfile:1

# ---------------------------------------------------------------------------
# Build stage
# ---------------------------------------------------------------------------
FROM golang:1.26-alpine AS build

WORKDIR /src

# Dependencies are copied first so the module cache layer survives source edits.
COPY go.mod go.su[m] ./
RUN go mod download

COPY . .

ARG VERSION=dev
ARG COMMIT=none
ARG BUILD_DATE=unknown
ARG TARGETOS=linux
ARG TARGETARCH

# CGO is disabled so the result is a static binary that runs on a bare image.
# -trimpath keeps build paths out of the binary.
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath \
      -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.buildDate=${BUILD_DATE}" \
      -o /out/api ./cmd/api

# ---------------------------------------------------------------------------
# Runtime stage
# ---------------------------------------------------------------------------
FROM alpine:3.21 AS runtime

# ca-certificates for outbound TLS; wget backs the container healthcheck.
RUN apk add --no-cache ca-certificates tzdata wget \
    && addgroup -g 65532 -S nonroot \
    && adduser -u 65532 -S -G nonroot -H -s /sbin/nologin nonroot

COPY --from=build /out/api /usr/local/bin/api

USER nonroot:nonroot
WORKDIR /home/nonroot

ENV HTTP_HOST=0.0.0.0 \
    HTTP_PORT=8080

EXPOSE 8080

# Liveness only: readiness is evaluated by the orchestrator against /readyz,
# because a failing dependency must not restart this container.
HEALTHCHECK --interval=10s --timeout=3s --start-period=5s --retries=3 \
    CMD wget --quiet --spider --tries=1 http://127.0.0.1:8080/healthz || exit 1

ENTRYPOINT ["/usr/local/bin/api"]
