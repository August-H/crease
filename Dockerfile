# syntax=docker/dockerfile:1
ARG GO_VERSION=1.26.4
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd/ ./cmd/
COPY internal/ ./internal/
ARG TARGETOS=linux
ARG TARGETARCH
ARG VERSION=0.1.0-dev
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/crease ./cmd/crease
RUN mkdir -p /out/data /out/config && chmod 0700 /out/data

FROM build AS test
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go test -race -timeout 120s ./...

FROM gcr.io/distroless/static-debian13:nonroot AS runtime
COPY --from=build /out/crease /usr/local/bin/crease
# Fresh named volumes inherit this directory's non-root ownership.
COPY --from=build --chown=65532:65532 /out/data/ /var/lib/crease/
COPY --from=build --chown=65532:65532 /out/config/ /etc/crease/
USER 65532:65532
EXPOSE 2222
STOPSIGNAL SIGTERM
ENTRYPOINT ["/usr/local/bin/crease"]
CMD ["serve", "--data", "/var/lib/crease", "--listen", ":2222", "--owner-key", "/etc/crease/owner.pub"]
