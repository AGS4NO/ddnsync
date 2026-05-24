# ddnsync

A tiny self-hosted DDNS service for DNSimple. Runs in either of two modes
(or both at once):

- **Server mode** — accepts noip / dyndns2 GET updates from any client that
  speaks that protocol (UniFi, inadyn, ddclient, custom curl scripts).
- **Poll mode** — periodically self-detects the WAN IP and updates a fixed
  list of records, with no inbound HTTP exposure at all.

Single static Go binary, stdlib only, distroless container.

## Why

For anyone who wants to run their own local DDNS service in front of DNSimple,
rather than depend on a hosted third-party shim. Your router keeps the DNS records up to date and the DNSimple credentials stay on a host you control.

## Configure

| Env var          | Required                | Default                       | Notes                                                            |
| ---------------- | ----------------------- | ----------------------------- | ---------------------------------------------------------------- |
| `DNSIMPLE_TOKEN` | yes                     |                               | DNSimple **account** access token                                |
| `LISTEN_ADDR`    | no                      | `:8245`                       | `host:port` to bind. Set `off` to disable the HTTP server.       |
| `AUTH_USER`      | server mode             |                               | Basic-auth username the router presents                          |
| `AUTH_PASS`      | server mode             |                               | Basic-auth password the router presents                          |
| `POLL_INTERVAL`  | no                      | (disabled)                    | e.g. `5m`. Minimum `30s`. Enables poll mode.                     |
| `POLL_HOSTNAMES` | poll mode               |                               | Comma-separated list, e.g. `home.example.com,vpn.example.com`    |
| `POLL_IP_SOURCE` | no                      | `https://api.ipify.org`       | URL that returns the public IP as plain text                     |
| `DNSIMPLE_API`   | no                      | `https://api.dnsimple.com`    | Use the sandbox URL for testing                                  |
| `RECORD_TTL`     | no                      | `60`                          | TTL (seconds) for newly created records                          |

At least one mode must be enabled. Both can run side-by-side. Account id and
zone list are auto-discovered on boot — restart after adding a new zone in
DNSimple.

## Modes

### Server mode (default)

ddnsync listens on `LISTEN_ADDR` for dyndns2-style updates. A router on the
LAN (or any noip-compatible client) calls `/nic/update` with basic auth and
ddnsync syncs the DNSimple record. This is the right pick if the router itself
will be doing the updating.

> **Most router DDNS clients require HTTPS.** ddnsync itself only speaks plain
> HTTP — the built-in DDNS clients in UniFi, OPNsense, pfSense, MikroTik, and
> most consumer routers refuse to talk plain HTTP to known providers (`dyndns`,
> `noip`, etc.), with no UI option to disable that. To use server mode with
> those routers you'll need to terminate TLS in front of ddnsync (Caddy +
> Let's Encrypt DNS-01 against DNSimple is the easiest path). If you don't
> want to deal with certificates, use **poll mode** instead.

### Poll mode

ddnsync runs an internal timer. Every `POLL_INTERVAL` it fetches the WAN IP
from `POLL_IP_SOURCE` and walks `POLL_HOSTNAMES`, calling DNSimple for each.
There is no inbound HTTP requirement, so there's no port to expose and no TLS
to set up — everything is outbound. This is the right pick for hosts behind
strict firewalls, or when the router can't be persuaded to do plain-HTTP DDNS
(modern UniFi firmware forces HTTPS on all built-in providers).

Set `LISTEN_ADDR=off` to disable the HTTP server entirely; `AUTH_USER` and
`AUTH_PASS` are not required in that case.

## Run

### From source

```sh
cp .env.example .env && $EDITOR .env
set -a && . ./.env && set +a
go run .
```

### Container

```sh
docker run --rm \
  --env-file .env \
  -p 8245:8245 \
  ghcr.io/ags4no/ddnsync:latest
```

Poll-only example (no inbound port):

```sh
docker run --rm \
  -e DNSIMPLE_TOKEN=… \
  -e LISTEN_ADDR=off \
  -e POLL_INTERVAL=5m \
  -e POLL_HOSTNAMES=home.example.com \
  ghcr.io/ags4no/ddnsync:latest
```

## Server-mode reference

```sh
curl -u "$AUTH_USER:$AUTH_PASS" \
  "http://localhost:8245/nic/update?hostname=home.example.com&myip=203.0.113.42"
```

If `myip` is omitted, the client's source address is used (or
`X-Forwarded-For` if you're behind a reverse proxy).

### Response codes (plain text, dyndns2 compatible)

| Body                  | Meaning                                            |
| --------------------- | -------------------------------------------------- |
| `good <ip>`           | Record was created or patched                      |
| `nochg <ip>`          | Record already pointed at this IP                  |
| `badauth`             | Wrong basic-auth credentials (also HTTP 401)       |
| `nohost`              | `hostname` missing, or no DNSimple zone matches it |
| `911`                 | DNSimple API call failed — check server logs       |

### Endpoints

- `GET /nic/update` — main update endpoint
- `GET /v2/update` — alias some clients prefer
- `GET /healthz` — unauthenticated `ok` for uptime checks

### UniFi config

In the UniFi controller, configure a Dynamic DNS entry with:

- **Service**: `dyndns`
- **Hostname**: e.g. `home.example.com`
- **Username**: your `AUTH_USER`
- **Password**: your `AUTH_PASS`
- **Server**: bare hostname of your ddnsync instance, e.g. `ddnsync.example.net`
  (no path, no `%h`/`%i` — the `dyndns` service template appends
  `/nic/update?hostname=...&myip=...` automatically)

UniFi will only talk HTTPS — see the TLS note above.

## Limitations

- Single tenant (one DNSimple token, one basic-auth credential).
- Zone list is cached at startup; restart to pick up new zones.
- No CNAME/MX/etc. handling — A and AAAA only.
- If a non-A/AAAA record already exists at the target name, DNSimple rejects
  the create and ddnsync returns `911` with the error in its log.