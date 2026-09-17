# Build natively, cross-compile the runtime for the Proxy's Linux/amd64 platform.
FROM --platform=$BUILDPLATFORM golang:1.26-bookworm AS build
WORKDIR /src
COPY dbm-services/common/dbha-v2/go.mod dbm-services/common/dbha-v2/go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY dbm-services/common/dbha-v2/ ./
COPY dbm-services/common/go-pubpkg/ /go-pubpkg/
RUN go work init . /go-pubpkg
ARG TARGETARCH=amd64
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH go build -trimpath -o /out/dbha-admin ./cmd/admin && \
    CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH go build -trimpath -o /out/dbha-receiver ./cmd/receiver && \
    CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH go build -trimpath -o /out/dbha-analysis ./cmd/analysis && \
    CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH go build -trimpath -o /out/dbha-probe ./cmd/probe && \
    CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH go build -trimpath -o /out/standalone-metadata ./tools/cmd/standalone-metadata

FROM debian:bookworm-slim AS runtime
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl \
    && rm -rf /var/lib/apt/lists/* && mkdir -p /run/dbha /var/log/dbha
COPY --from=build /out/ /usr/local/bin/
WORKDIR /etc/dbha
