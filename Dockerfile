# syntax=docker/dockerfile:1

# ---------------------------------------------------------------------------
# Stage 1: build the React frontend. Its output is platform-independent, so the
# stage is pinned to the build host and never runs under emulation.
FROM --platform=$BUILDPLATFORM node:22-alpine@sha256:c610fcdfb1d5b4740dd70c284ed3cb16bb857e0f7166196e36a5501df7a3aa32 AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
# vite.config.ts outputs to ../internal/web/dist → /src/internal/web/dist
RUN npm run build

# ---------------------------------------------------------------------------
# Stage 2: compile the Go binary with the frontend embedded. Also pinned to the
# build host: Go cross-compiles for the target platform natively, which is far
# faster and more reliable than emulating the toolchain under QEMU.
FROM --platform=$BUILDPLATFORM golang:1.26.6-alpine@sha256:3889b425f035be855a72fb4755265311293b6d414521f0a519d819df32222d83 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /src/internal/web/dist internal/web/dist

# BuildKit populates these from the platform being built for.
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=0.1.0-dev
ARG COMMIT=unknown
ARG DATE=unknown
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
    -ldflags "-s -w \
      -X github.com/openconvo/openconvo/internal/version.Version=${VERSION} \
      -X github.com/openconvo/openconvo/internal/version.Commit=${COMMIT} \
      -X github.com/openconvo/openconvo/internal/version.Date=${DATE}" \
    -o /out/openconvo ./cmd/openconvo

# ---------------------------------------------------------------------------
# Stage 3: minimal runtime. No Node or compiler; pg_dump is the one operational
# tool, used for application-managed logical database backups.
#
# Base images are pinned by digest; Dependabot proposes digest updates. The
# `apk upgrade` picks up Alpine security fixes published since the base image
# was built, so a rebuild is a complete remedy for an OS package advisory.
FROM alpine:3.22@sha256:14358309a308569c32bdc37e2e0e9694be33a9d99e68afb0f5ff33cc1f695dce

RUN apk --no-cache upgrade \
    && apk add --no-cache ca-certificates tzdata postgresql17-client \
    && addgroup -S openconvo \
    && adduser -S -G openconvo -h /home/openconvo openconvo \
    && mkdir -p /data/attachments \
    && chown -R openconvo:openconvo /data

COPY --from=build /out/openconvo /usr/local/bin/openconvo

USER openconvo
ENV STORAGE_DRIVER=filesystem \
    STORAGE_PATH=/data/attachments

EXPOSE 8080
VOLUME /data

HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
    CMD ["openconvo", "healthcheck"]

ENTRYPOINT ["openconvo"]
CMD ["serve"]
