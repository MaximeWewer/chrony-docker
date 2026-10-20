# chrony-docker

Hardened [chrony](https://chrony-project.org/) image to run an NTP/NTS server. It runs as non-root on `gcr.io/distroless/static-debian13:nonroot`, over IPv4 and IPv6.

- chrony 4.9 built as a **static PIE** binary (musl, full RELRO, stack protector, FORTIFY).
- **NTS** client and server. GnuTLS 3.8 is compiled statically, because no distribution ships it that way.
- **No Linux capabilities** required, works with a read-only root filesystem.
- chronyd's own **seccomp filter** is enabled (`-F 1`), on top of the runtime's default profile.
- No shell in the image. A small static Go entrypoint renders the configuration, then `exec`s chronyd, which becomes PID 1.
- The UDP command port is disabled (`cmdport 0`): `chronyc` only works through a local Unix socket.

For Kubernetes, use the Helm chart: **[MaximeWewer/chrony-helm](https://github.com/MaximeWewer/chrony-helm)**.

## Docker

```bash
docker run -d --name chrony --restart unless-stopped \
  --read-only --cap-drop ALL --security-opt no-new-privileges \
  --sysctl net.ipv4.ip_unprivileged_port_start=123 \
  -v chrony-state:/var/lib/chrony \
  -p 123:123/udp -p 4460:4460/tcp \
  -e NTP_SERVERS=time.cloudflare.com,nts.netnod.se,ptbtime1.ptb.de \
  -e NTS=true -e MINSOURCES=2 \
  -e ALLOW=192.168.0.0/16,fd00::/8 \
  ghcr.io/maximewewer/chrony-docker:latest
```

- `--sysctl net.ipv4.ip_unprivileged_port_start=123` lets uid 65532 bind port 123 without `CAP_NET_BIND_SERVICE`. You can also keep the default and publish a high port instead: `-e NTP_PORT=1123 -p 123:1123/udp`.
- The state volume (or a `--tmpfs /var/lib/chrony:uid=65532,gid=65532,mode=0700`) is the only writable path.
- Without `ALLOW`, the server answers localhost only.

## Docker Compose

See [`compose.yaml`](compose.yaml):

```bash
docker compose up -d
docker compose exec chrony /usr/bin/chronyc -h /var/lib/chrony/chronyd.sock -N sources
```

## Operations

There is no shell. Everything goes through the entrypoint or `chronyc`:

```bash
docker run --rm ghcr.io/maximewewer/chrony-docker config   # render, validate with `chronyd -p`, print
docker exec chrony /chrony-entrypoint version
docker exec chrony /chrony-entrypoint health                # liveness
docker exec chrony /chrony-entrypoint ready                 # readiness
docker exec chrony /usr/bin/chronyc -h /var/lib/chrony/chronyd.sock tracking
docker exec chrony /usr/bin/chronyc -h /var/lib/chrony/chronyd.sock -N authdata
docker exec chrony /usr/bin/chronyc -h /var/lib/chrony/chronyd.sock reload sources
```

- **`health`** checks only that chronyd has bound its port. It does **not** require synchronisation: chronyd does not answer clients until it is synchronised, and a strict liveness check would restart every instance during an upstream outage. It is the image's Docker `HEALTHCHECK`.
- **`ready`** sends a real NTP request to localhost. It requires `leap ≠ 3`, a valid stratum and a root distance below `READY_MAX_ROOT_DISTANCE`, and checks the NTS-KE port when a certificate is mounted. In client-only mode (`NTP_PORT=0`) it uses `chronyc tracking` instead.
- Both try `127.0.0.1` then `::1`, so they work on IPv4-only, IPv6-only and dual-stack hosts.

## NTS server

Mount a certificate and its key, readable by uid 65532 (mode `0440` is enough; chronyd warns about a world-readable key):

```bash
-v ./tls.crt:/etc/chrony/nts/tls.crt:ro -v ./tls.key:/etc/chrony/nts/tls.key:ro
```

NTS is enabled as soon as both files are present. Clients then use `server <name> iburst nts`.

**Several instances behind one address.** A client runs NTS-KE against one instance, then its NTP packets may reach another one. All instances must therefore share the cookie keys:

```bash
# 1. Generate an ntskeys file in chrony's format
docker volume create nts-seed
docker run -d --name seed -v nts-seed:/var/lib/chrony \
  -v "$PWD/tls.crt:/etc/chrony/nts/tls.crt:ro" -v "$PWD/tls.key:/etc/chrony/nts/tls.key:ro" \
  --sysctl net.ipv4.ip_unprivileged_port_start=123 ghcr.io/maximewewer/chrony-docker
sleep 3 && docker stop seed
docker run --rm -v nts-seed:/v alpine:3.24 cat /v/ntskeys > ntskeys
docker rm seed && docker volume rm nts-seed

# 2. Mount it read-only in every instance
-v ./ntskeys:/etc/chrony/nts/ntskeys:ro -e NTS_NTP_SERVER=ntp.example.com
```

- The entrypoint copies the file into the state directory and sets `ntsrotate 0`.
- To rotate the keys, generate a new file and restart the instances: clients redo NTS-KE on their own.
- `NTS_NTP_SERVER` tells clients which name or address to send NTP to.

## SNAT, multiple sites

**SNAT.** chronyd (`ALLOW`, `RATELIMIT`, `chronyc clients`) sees the source address _after_ SNAT.

- Keep the real client address if you can.
- Otherwise, put the SNAT ranges in `ALLOW` and leave `RATELIMIT` off: a per-address rate limit would block every client sharing that address.
- Amplification is not a concern. The command port is closed, chrony has no `monlist` and no ntpd mode 6/7, and an NTP or NTS response is no larger than its request.
- Time authenticity comes from NTS, not from the source address.

**Isolated site.** With `LOCAL_STRATUM=10` and `LOCAL_ORPHAN=true`, a site cut off from its upstreams keeps serving consistent time. Declare the servers of the other sites with `NTP_PEERS` or in `sources.d/`: in orphan mode, isolated sites agree with each other.

**Clock.** `-x` is on by default: chronyd does not touch the host clock, which stays managed by the host, and serves the corrected time it computes.

## Environment variables

Lists accept commas or newlines as separators. A source entry that contains spaces is used verbatim, which allows per-source options (`ntp1.example.com minpoll 4 prefer nts`).

### Sources

| Variable                                                   | Default                       | Effect                                                                                |
| ---------------------------------------------------------- | ----------------------------- | ------------------------------------------------------------------------------------- |
| `NTP_SERVERS` / `NTP_POOLS` / `NTP_PEERS`                  | —                             | `server` / `pool` / `peer`                                                            |
| `SOURCE_OPTIONS`                                           | `iburst minpoll 6 maxpoll 10` | options added to entries that have none                                               |
| `NTS`                                                      | `false`                       | adds `nts` to every source. When all sources use NTS, `authselectmode require` is set |
| `NTS_TRUSTED_CERTS`                                        | —                             | `ntstrustedcerts` (private CA)                                                        |
| `REFCLOCKS`                                                | —                             | `refclock` lines (PHC/PPS)                                                            |
| `MINSOURCES`, `MAXDISTANCE`, `MAXJITTER`, `AUTHSELECTMODE` | —                             | source selection                                                                      |
| `LOCAL_STRATUM`, `LOCAL_ORPHAN`, `LOCAL_DISTANCE`          | —                             | `local stratum N [orphan] [distance D]`                                               |

### Server

| Variable                        | Default      | Effect                                                                                                                                         |
| ------------------------------- | ------------ | ---------------------------------------------------------------------------------------------------------------------------------------------- |
| `NTP_PORT`                      | `123`        | `port`. `0` = client only                                                                                                                      |
| `BIND_ADDRESS`                  | —            | `bindaddress` (IPv4 and/or IPv6)                                                                                                               |
| `ALLOW` / `DENY`                | —            | `allow` / `deny`. chrony applies the most specific subnet, not the line order. `127.0.0.1` and `::1` are always allowed, for the health checks |
| `RATELIMIT` / `NTS_RATELIMIT`   | off          | `on` (= `interval 3 burst 8 leak 2`) or raw parameters                                                                                         |
| `CLIENTLOGLIMIT`, `NOCLIENTLOG` | —            | client log                                                                                                                                     |
| `SMOOTHTIME`                    | —            | time smoothing, e.g. `400 0.001 leaponly` for a leap smear                                                                                     |
| `LEAPSECLIST`                   | bundled file | `off` to disable, or the path of a newer mounted file                                                                                          |
| `LEAPSECMODE`                   | —            | `leapsecmode`                                                                                                                                  |
| `IP_MODE`                       | dual stack   | `4` or `6` to use a single family (`chronyd -4/-6`)                                                                                            |

### NTS server

| Variable                             | Default                                          | Effect                                                       |
| ------------------------------------ | ------------------------------------------------ | ------------------------------------------------------------ |
| `NTS_SERVER_CERT` / `NTS_SERVER_KEY` | `/etc/chrony/nts/tls.crt` / `tls.key` if present | paired lists, several certificates allowed                   |
| `NTS_PORT`                           | `4460`                                           | `ntsport`                                                    |
| `NTS_NTP_SERVER`                     | —                                                | `ntsntpserver`: NTP name or address given to clients         |
| `NTS_KEYS_FILE`                      | `/etc/chrony/nts/ntskeys` if present             | copied into the state directory at start, with `ntsrotate 0` |
| `NTS_ROTATE`                         | —                                                | forces `ntsrotate`                                           |

### Runtime

| Variable                                              | Default                           | Effect                                                                             |
| ----------------------------------------------------- | --------------------------------- | ---------------------------------------------------------------------------------- |
| `CHRONY_CONF`                                         | —                                 | full mounted config, used as-is (the environment is ignored)                       |
| `CHRONY_CONF_DIR`                                     | `/etc/chrony/conf.d`              | `confdir` if it exists (`*.conf` files)                                            |
| `CHRONY_SOURCE_DIR`                                   | `/etc/chrony/sources.d`           | `sourcedir` if it exists (`*.sources` files, reloaded by `chronyc reload sources`) |
| `EXTRA_CONF`                                          | —                                 | raw directives appended at the end                                                 |
| `CLOCK_CONTROL`                                       | `false`                           | `true` drops `-x`. Needs `CAP_SYS_TIME`, avoid it                                  |
| `SCFILTER_LEVEL`                                      | `1`                               | `chronyd -F`. `2` is more permissive, `0` disables the filter                      |
| `LOG_LEVEL`, `LOGS`, `EXTRA_ARGS`                     | —                                 | `-L`, `log …`, extra chronyd arguments                                             |
| `READY_MAX_ROOT_DISTANCE`                             | `1`                               | readiness threshold, in seconds                                                    |
| `HEALTH_MAX_STRATUM`, `HEALTH_TIMEOUT`, `HEALTH_ADDR` | `15`, `3`, `127.0.0.1` then `::1` | health checks                                                                      |

## Licenses

The content of this repository (Dockerfile, entrypoint, workflows) is under the MIT license. The image contains software under its own licenses: chrony (GPL-2.0-only), GnuTLS (LGPL-2.1-or-later), nettle and GMP (LGPL-3.0-or-later or GPL-2.0-or-later), musl (MIT) and libseccomp (LGPL-2.1). Because these are statically linked, redistributing the image must allow rebuilding it with other versions of those libraries. The Dockerfile, which rebuilds everything from the published sources, serves that purpose.
