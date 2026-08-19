# Cross-compilation happens natively on the build host: the builder always runs
# on $BUILDPLATFORM and targets $TARGETPLATFORM via GOOS/GOARCH. No QEMU is
# involved, so an arm64 image builds at native speed on an amd64 runner.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build

WORKDIR /src

# Dependencies are their own layer so source edits do not re-download the module
# graph. The cache mount keeps them across builds too.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=none

# CGO is off because every dependency is pure Go: pgx speaks the wire protocol
# directly and TLS comes from crypto/tls. That is what allows a static binary on
# a distroless base with no libc at all.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath \
      -ldflags="-s -w -X github.com/esaiaswestberg/imapped/internal/cli.Version=${VERSION} -X github.com/esaiaswestberg/imapped/internal/cli.Commit=${COMMIT}" \
      -o /out/imapped ./cmd/imapped

# distroless/static provides CA certificates and an /etc/passwd entry for the
# nonroot user, and nothing else — no shell, no package manager.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/imapped /imapped

# 1143 IMAP, 1993 IMAPS, 8080 web UI, 9090 metrics.
EXPOSE 1143 1993 8080 9090

USER nonroot:nonroot
ENTRYPOINT ["/imapped"]
CMD ["run"]
