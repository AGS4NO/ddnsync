# ddnsync

A tiny noip-compatible DDNS shim that translates `dyndns2`-style HTTP updates
into authenticated DNSimple v2 API calls. Point a UniFi router (or any client
that speaks the No-IP / dyndns2 protocol) at ddnsync, and it keeps a DNSimple
A/AAAA record in sync with whatever IP the client reports.

Single static Go binary, stdlib only, distroless container.

## Why

For anyone who wants to run their own local DDNS service in front of DNSimple,
rather than depend on a hosted third-party shim. Your router keeps speaking
the standard dyndns2 protocol; the DNSimple credentials stay on a host you
control.

## Configure

| Env var               | Required | Default                       | Notes                                          |
| --------------------- | -------- | ----------------------------- | ---------------------------------------------- |
| `DNSIMPLE_TOKEN`      | yes      |                               | DNSimple **account** access token              |
| `AUTH_USER`           | yes      |                               | Basic-auth username the router presents        |
| `AUTH_PASS`           | yes      |                               | Basic-auth password the router presents        |
| `LISTEN_ADDR`         | no       | `:8245`                       | `host:port` to bind                            |
| `DNSIMPLE_API`        | no       | `https://api.dnsimple.com`    | Use the sandbox URL for testing                |
| `RECORD_TTL`          | no       | `60`                          | TTL (seconds) for newly created records        |

The account id is auto-discovered via `GET /v2/whoami` on boot. The set of
zones is also cached on boot — restart after adding a new zone in DNSimple.

## Run

### From source

```sh
cp .env.example .env && $EDITOR .env
set -a && . ./.env && set +a
go run .
```

### Container

```sh
docker build -t ddnsync .
docker run --rm -p 8245:8245 --env-file .env ddnsync
```

## Use

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

In the UniFi controller, configure a "dyndns" Dynamic DNS entry with:

- **Service**: `dyndns`
- **Hostname**: e.g. `home.example.com`
- **Username/Password**: your `AUTH_USER` / `AUTH_PASS`
- **Server**: `your-ddnsync-host/nic/update?hostname=%h&myip=%i`

## Limitations

- Single tenant (one DNSimple token, one basic-auth credential).
- Zone list is cached at startup; restart to pick up new zones.
- No CNAME/MX/etc. handling — A and AAAA only.
- If a non-A/AAAA record already exists at the target name, DNSimple rejects
  the create and ddnsync returns `911` with the error in its log.