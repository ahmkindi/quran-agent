# Production deployment (single VM + domain)

Target: one Linux VM (~2 GB RAM) with a **public IP directly on its interface**
and a domain. Everything runs from this repo with docker compose; Caddy gets
TLS certs automatically from Let's Encrypt.

```
Browser ──HTTPS/WSS──► Caddy :443 /*     ──► web :8080 (loopback)   UI + /ws + /rtc-config
        ──WSS───────► Caddy :443 /rtc*  ──► livekit :7880           LiveKit signaling
        ──WebRTC────► livekit :7881/tcp :7882/udp                   LiveKit media (direct)
        ──WebRTC────► web (host net, ephemeral UDP + srflx)         pion media (direct)
        ──TURN──────► coturn :3478 (relay 49160–49960/udp)          fallback for strict NATs
```

## 1. DNS

One A record → the VM's public IP (`APP_DOMAIN`, e.g. `quran.kindi.dev`).
LiveKit signaling shares it: the livekit-client SDK talks to `{url}/rtc`, so
Caddy routes `/rtc*` to livekit and the rest to web. TURN needs no DNS —
`TURN_HOST` may be the bare VM IP (port 3478 is raw UDP/TCP, no TLS).

The record must not be behind a proxying CDN (no Cloudflare orange cloud) —
WebRTC needs the real IP.

## 2. Firewall (open inbound)

| Port | Proto | Service |
|---|---|---|
| 22 | tcp | SSH |
| 80, 443 | tcp | Caddy (ACME + HTTPS/WSS) |
| 7881 | tcp | LiveKit ICE/TCP |
| 7882 | udp | LiveKit media (single-port UDP mux) |
| 3478 | udp+tcp | coturn TURN |
| 49160–49960 | udp | coturn relay range |
| 50700–50900 | udp | pion ICE media (`PION_UDP_PORT_RANGE`) |

The pion range is mandatory with a default-deny firewall: clients behind
symmetric NAT (mobile CGNAT) send connectivity checks from ports the server
has never contacted, so conntrack won't pass them — pion must be reachable on
its pinned range to answer and form prflx pairs. If the VM's provider firewall
(cloud security group) exists, open the same ports there too.

## 3. VM prerequisites

- Docker + docker compose plugin.
- Clone the repo; `git pull` to deploy updates.

## 4. Secrets / `.env` on the VM

`cp .env.example .env`, then set:

```bash
GOOGLE_API_KEY=...                    # aistudio.google.com/apikey

APP_DOMAIN=quran.kindi.dev
TURN_HOST=<VM public IP>              # or a domain; bare IP is fine for TURN
ACME_EMAIL=you@example.com

LIVEKIT_NODE_IP=<VM public IP>        # LiveKit advertises this in candidates
LIVEKIT_API_KEY=$(head -c9 /dev/urandom | base64 | tr -dc a-zA-Z0-9)
LIVEKIT_API_SECRET=$(openssl rand -base64 32 | tr -d '=+/')
TURN_SECRET=$(openssl rand -base64 32 | tr -d '=+/')

COMPOSE_PROFILES=turn                 # coturn always on in prod
```

Never commit `.env`. Rotating `TURN_SECRET`/LiveKit keys = edit `.env` +
`docker compose ... up -d` (recreates only the changed services).

If the VM's public IP is **NAT-mapped** (interface shows a private IP —
`ip -4 addr` doesn't list the public one), add `--external-ip=<public>/<private>`
to the coturn command in `docker-compose.prod.yml`, and keep
`LIVEKIT_NODE_IP=<public>` (LiveKit validates it by self-ping).

## 5. Bring-up

```bash
docker compose -f docker-compose.yml -f docker-compose.prod.yml up -d --build
docker compose -f docker-compose.yml -f docker-compose.prod.yml ps
docker compose -f docker-compose.yml -f docker-compose.prod.yml logs -f caddy
```

Caddy logs show certificate issuance for both domains within ~30 s (needs 80
and 443 reachable). `make prod` / `make prod-down` wrap the same commands.

## 6. Verify (in order)

1. `https://app.<domain>` loads (green padlock).
2. `curl https://app.<domain>/rtc-config` → JSON with `stun:` + two `turn:` URLs
   and ephemeral credentials.
3. **From a phone on mobile data** (real NAT, not your Wi-Fi):
   - `/pion`: call, talk, interrupt, HUD shows perceived latency.
   - `/livekit`: same. This is LiveKit's standard deployment shape
     (public IP srflx) — the local-LAN mDNS pathology does not apply here.
4. **TURN proof**: set `FORCE_RELAY=true` in the VM `.env`, recreate web
   (`... up -d web`), call again from the phone — both pages must still work;
   `docker compose ... logs coturn` shows allocations (logs to stdout).
   Set back to `false` afterwards (relay adds a hop; direct is faster).
5. Latency: agent logs (`docker compose ... logs agent | grep gemini_ms`) for
   per-turn numbers; HUD for perceived. Jaeger stays off in prod unless
   `OTEL_EXPORTER_OTLP_ENDPOINT` is set and the `obs` profile is added.

## 7. Update / operate

```bash
git pull
docker compose -f docker-compose.yml -f docker-compose.prod.yml up -d --build
```

- Logs are JSON (`LOG_FORMAT=json`); `jq` the compose logs.
- Memory budget (~2 GB VM): agent 640M + web 640M limits, LiveKit/coturn/Caddy
  are small at demo load; Postgres defaults.
- All state that matters: `pgdata` volume (Postgres) + `caddy_data` (certs).
