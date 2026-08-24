# syntax=docker/dockerfile:1.7

FROM golang:1.25-alpine AS build

ARG SERVICE
ARG SERVICE_PATH

WORKDIR /src

COPY go.* ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .

RUN --mount=type=cache,target=/root/.cache/go-build \
    case "${SERVICE}" in \
        payment) default_path="./services/payment/cmd/api" ;; \
        payment-worker) default_path="./services/payment/cmd/worker" ;; \
        router) default_path="./services/router/cmd/server" ;; \
        gateway) default_path="./services/provider-gateway/cmd/server" ;; \
        simulator) default_path="./tools/provider-simulator/cmd/server" ;; \
        *) echo "unsupported SERVICE: ${SERVICE}" >&2; exit 1 ;; \
    esac && \
    build_path="${SERVICE_PATH:-${default_path}}" && \
    test -d "${build_path}" && \
    CGO_ENABLED=0 GOOS=linux go build -trimpath \
        -ldflags="-s -w" \
        -o /out/service "${build_path}"

FROM alpine:3.22

RUN apk add --no-cache ca-certificates tzdata && \
    addgroup -S -g 65532 payments && \
    adduser -S -D -H -u 65532 -G payments payments

COPY --from=build /out/service /usr/local/bin/service

USER 65532:65532

ENTRYPOINT ["/usr/local/bin/service"]
