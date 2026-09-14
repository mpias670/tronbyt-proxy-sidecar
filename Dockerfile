# syntax=docker/dockerfile:1

# ---- build stage ----------------------------------------------------------
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# CGO disabled + a static binary keeps the final image tiny and avoids any
# libc dependency mismatches between the build and runtime base images.
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /out/sidecar .

# ---- runtime stage ----------------------------------------------------------
# Chromium is only ever launched on demand for the handful of seconds it
# takes to solve a Cloudflare challenge, then killed -- so its disk footprint
# here does not translate into steady-state memory usage. Steady-state RSS
# is just the Go binary (a few MB) sitting idle between requests.
FROM alpine:3.20
RUN apk add --no-cache chromium ca-certificates tzdata \
    && rm -rf /var/cache/apk/*

ENV CHROME_PATH=/usr/bin/chromium-browser \
    PORT=8080

WORKDIR /app
COPY --from=build /out/sidecar /app/sidecar

RUN addgroup -S sidecar && adduser -S -G sidecar sidecar
USER sidecar

EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s \
    CMD wget -qO- http://127.0.0.1:8080/healthz || exit 1

ENTRYPOINT ["/app/sidecar"]
