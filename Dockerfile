# syntax=docker/dockerfile:1

# ---------------------------------------------------------------------------
# Stage 1: build the React frontend.
FROM node:22-alpine AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
# vite.config.ts outputs to ../internal/web/dist → /src/internal/web/dist
RUN npm run build

# ---------------------------------------------------------------------------
# Stage 2: compile the Go binary with the frontend embedded.
FROM golang:1.26.6-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /src/internal/web/dist internal/web/dist

ARG VERSION=0.1.0-dev
ARG COMMIT=unknown
ARG DATE=unknown
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w \
      -X github.com/openconvo/openconvo/internal/version.Version=${VERSION} \
      -X github.com/openconvo/openconvo/internal/version.Commit=${COMMIT} \
      -X github.com/openconvo/openconvo/internal/version.Date=${DATE}" \
    -o /out/openconvo ./cmd/openconvo

# ---------------------------------------------------------------------------
# Stage 3: minimal runtime. No Node or compiler; pg_dump is the one operational
# tool, used for application-managed logical database backups.
FROM alpine:3.22

RUN apk add --no-cache ca-certificates tzdata postgresql17-client \
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
