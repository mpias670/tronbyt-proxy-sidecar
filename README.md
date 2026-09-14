# Tronbyt Proxy Sidecar

A small, generic HTTP proxy that fetches URLs on behalf of a Tronbyt (pixlet)
app and transparently handles Cloudflare's bot protection, so the app itself
can stay a simple Starlark `http.get()` call. Built primarily to unblock the
[`surflive-proxy`](https://github.com/tronbyt/apps/tree/main/apps/surflive-proxy)
app against Surfline's API, but it's domain-agnostic — point it at any
Cloudflare-protected JSON API.

```
Tronbyt App (pixlet)  --http.get()-->  sidecar  --bypasses Cloudflare-->  Upstream API
```

## Why this exists

Surfline's API sits behind Cloudflare bot management that blocks non-browser
HTTP clients outright, even with spoofed browser headers. Pixlet apps run in
a sandboxed Starlark interpreter with no ability to run a browser or a
custom TLS stack, so bypassing this has to happen in a separate service.
Running a full always-on Playwright/Chromium instance to do this is memory-
heavy for something that idles almost all the time. This sidecar instead:

1. **Tries a TLS-impersonated request first** (the "fast path"). Using
   [uTLS](https://github.com/refraction-networking/utls), it performs a TLS
   handshake that fingerprints as real Chrome (JA3), which alone defeats a
   meaningful fraction of Cloudflare's bot-management heuristics — no
   browser involved. In testing against Surfline's API, this path alone
   succeeded every time.
2. **Falls back to a short-lived headless Chromium only if challenged.** If
   (and only if) the fast path gets a Cloudflare challenge page back, the
   sidecar launches headless Chromium just long enough to solve the
   challenge, extracts the resulting `cf_clearance` cookie + User-Agent, and
   **kills the browser immediately**. It never stays running between
   requests.
3. **Caches the solved session per domain** (in memory, with a TTL) so a
   browser solve, when it does happen, is amortized across many subsequent
   requests rather than repeated per-request.

## Memory profile

Measured with `docker stats` on this image:

| State | Memory |
|---|---|
| Idle (no requests yet) | ~2 MB |
| After fast-path-only requests | ~10-15 MB |
| During a browser fallback solve | ~150-250 MB (Chromium, running for a few seconds) |
| After the solve completes | back to ~10-15 MB *resident* (see note) |

**Note on `docker stats`:** after a browser solve, `docker stats`/cgroup
`memory.current` may still report an elevated number (~200MB+) even though
the Chromium process has fully exited. This is Linux's page cache holding
onto Chromium's binary/shared-library pages it read from disk, which is
reclaimable on demand and not real usage. Check `anon` (actual anonymous/
resident memory) in `/sys/fs/cgroup/memory.stat` for the true figure — in
testing this drops back to single-digit MB immediately after the browser
process exits. Set a `deploy.resources.limits.memory` cap in
`docker-compose.yml` for peace of mind regardless.

## Quick start

```sh
cp .env.example .env
# edit .env: set ALLOWED_DOMAINS, AUTH_TOKEN, etc.
# if port 8080 is already taken on your machine, just set HOST_PORT in .env
# (e.g. HOST_PORT=18080) -- no need to edit docker-compose.yml.
docker compose up -d --build
curl "http://localhost:8080/fetch?url=https://services.surfline.com/kbyg/spots/forecasts/wave?spotId=5842041f4e65fad6a7708841&intervalHours=1&days=2"
```

If you see `Error response from daemon: ... address already in use`, something
else already owns that host port. Either stop it (`docker ps -a` to find
stale containers, `docker compose down --remove-orphans` to clean up a prior
run of this stack) or just pick a different `HOST_PORT` in `.env`.

This repo's `docker-compose.yml` (with a published `HOST_PORT`) is meant for
local development/testing standalone. For actual use, colocate the sidecar
with your tronbyt-server instead (see below) so no host port needs to be
exposed at all.

## Production deployment: colocate with tronbyt-server

Add the sidecar as a service directly in your **tronbyt-server's own**
compose file, on the same network, so the app can reach it by container name
with no host port published:

```yaml
services:
  tronbyt:
    image: ghcr.io/tronbyt/server:2.3.2
    # ...your existing tronbyt-server config...
    networks:
      - homelab

  tronbyt-sidecar:
    image: tronbyt-sidecar:latest   # build this image from this directory first
    container_name: tronbyt-sidecar
    restart: unless-stopped
    env_file:
      - /path/to/sidecar.env        # copy .env.example here and edit it
    init: true
    networks:
      - homelab
    healthcheck:
      test: ["CMD", "wget", "-qO-", "http://127.0.0.1:8080/healthz"]
      interval: 30s
      timeout: 3s
      start_period: 5s
      retries: 3
    deploy:
      resources:
        limits:
          memory: 512M

networks:
  homelab:
    external: true
```

Build the image once from this directory (`docker build -t tronbyt-sidecar:latest .`),
then `docker compose up -d` from your tronbyt-server's compose project. The
`surflive-proxy` app's default `Proxy Sidecar URL` (`http://tronbyt-sidecar:8080`)
matches this container name out of the box — no LAN IP or extra port mapping
needed, since both containers share the same private Docker network.

## API

### `GET /fetch?url=<url-encoded target URL>`

Fetches `url` (must be `http`/`https` and match `ALLOWED_DOMAINS`), handling
Cloudflare bot protection transparently, and returns the upstream response's
body and status code verbatim (JSON in, JSON out — no wrapper envelope, so
callers can `.json()` the response directly).

A successful (2xx) response is cached in memory, keyed by the full target
URL, for `RESPONSE_CACHE_TTL_SECONDS` (default 15 min). This is the primary
defense against a busy pixlet render loop re-hitting Surfline every cycle:
even if the calling app's own `http.get(ttl_seconds=...)` cache doesn't hit
(e.g. it's disabled, or the render pipeline doesn't share a cache across
invocations), the sidecar itself won't re-fetch the same URL from Surfline
within the TTL. Responses include an `X-Sidecar-Cache: hit`/`miss` header so
you can confirm this in practice.

Headers:
- `X-Proxy-Token` — required if `AUTH_TOKEN` is set server-side; must match.

Responses:
- `200` + upstream body — success (including the upstream's own status if
  you want to check for e.g. Surfline 4xx errors — currently the sidecar
  always uses the actual upstream status code, except when it itself fails).
- `400` — missing/invalid `url` parameter.
- `401` — missing/incorrect `X-Proxy-Token`.
- `403` — `url`'s host isn't in `ALLOWED_DOMAINS`.
- `502` — upstream unreachable, or a Cloudflare challenge could not be
  solved (including via the browser fallback).

### `GET /healthz`

Returns `200 ok`. Used by the Docker healthcheck.

## Configuration

All configuration is via environment variables — see [`.env.example`](.env.example)
for the full list with defaults and explanations:

| Variable | Default | Purpose |
|---|---|---|
| `PORT` | `8080` | listen port |
| `ALLOWED_DOMAINS` | `surfline.com` | comma-separated allowlist; **required** for any domain you actually want to reach |
| `AUTH_TOKEN` | *(unset)* | shared secret for `X-Proxy-Token`; strongly recommended outside a fully trusted network |
| `BROWSER_CACHE_TTL_SECONDS` | `1500` (25 min) | how long a solved session is reused |
| `BROWSER_TIMEOUT_SECONDS` | `25` | hard timeout for one browser solve |
| `MAX_CONCURRENT_BROWSERS` | `1` | caps peak memory from the fallback path |
| `UPSTREAM_TIMEOUT_SECONDS` | `15` | timeout for the fast-path request |
| `RESPONSE_CACHE_TTL_SECONDS` | `900` (15 min) | how long a successful response body is cached per target URL; this is what keeps a busy render loop from re-hitting Surfline every cycle |
| `DISABLE_BROWSER_FALLBACK` | `false` | fast-path only, no Chromium at all |

## Security notes

- **This is a generic proxy** — anything reachable by callers can, in
  principle, be relayed through it to any allowed domain. Always set
  `ALLOWED_DOMAINS` to only what you need, and set `AUTH_TOKEN` unless the
  sidecar is on a network you fully trust (e.g. an isolated docker-compose
  network with no other untrusted containers).
- Upstream response bodies are capped at 10MB to protect the sidecar's own
  memory from a huge or malicious response.

## Wiring it up to a Tronbyt app

See [`surflive-proxy`](https://github.com/tronbyt/apps/tree/main/apps/surflive-proxy)
for a complete example app that calls this sidecar's `/fetch` endpoint instead
of Surfline directly, configured via a `Proxy Sidecar URL` / `Proxy Auth
Token` schema field pair.

## Development

Requires Go 1.24+.

```sh
go build ./...
go vet ./...
gofmt -l .

# Run locally without Docker (Chromium fallback needs a system Chrome/Chromium
# install; set CHROME_PATH if it's not auto-discovered, or set
# DISABLE_BROWSER_FALLBACK=true to skip that path entirely):
ALLOWED_DOMAINS=surfline.com go run .
```
