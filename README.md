# WhatsApp Sniper Bot — Go + whatsmeow

A low-latency, crypto-stable WhatsApp reaction bot. Monitors a specific group 24/7 and reacts with ✅ to messages containing configured keywords. Built to eliminate the **"Ghost Reaction"** failure mode that plagues Baileys-based bots.

## Why this replaces the Baileys version

| | Old (Node + Baileys) | **New (Go + whatsmeow)** |
|---|---|---|
| Signal state | File-per-key, non-atomic writes | Postgres transactions, atomic ratchet updates |
| Bad MAC recovery | Global nuke (caused Ghost Reactions) | Surgical, protocol-correct |
| Idle RAM | ~100 MB | ~25 MB |
| Cold start | 8–15 s | 1–2 s |
| Image size | ~350 MB | ~14 MB (scratch) |
| Session across restarts | Lost on Render ephemeral disk | Survives via Neon Postgres |

## Project layout

```
.
├── cmd/sniper/main.go              entry point, wiring, shutdown
├── internal/
│   ├── config/config.go            env loading + validation
│   ├── sniper/
│   │   ├── sniper.go               lifecycle, client, metrics, slog adapter
│   │   ├── handler.go              hot-path (detect → dispatch reaction)
│   │   ├── control.go              owner commands + own-msg-id echo guard
│   │   └── pairing.go              QR channel consumer
│   └── httpd/server.go             /health, /status, /pair endpoints
├── Dockerfile                      multi-stage → scratch image (~14 MB)
├── render.yaml                     Render IaC
├── go.mod
├── .gitignore
├── .dockerignore
└── README.md
```

## Required infrastructure (all free-tier)

1. **Neon Postgres** — session storage (<https://neon.tech>)
2. **Render Web Service** — runs the container (<https://render.com>)
3. **UptimeRobot** — pings `/health` every 5 min to prevent Render free-tier spin-down (<https://uptimerobot.com>)

## Environment variables

| Variable | Required | Example | Purpose |
|---|---|---|---|
| `DATABASE_URL` | yes | `postgresql://user:pass@ep-x-pooler.region.aws.neon.tech/neondb?sslmode=require` | Neon pooled connection string |
| `PAIR_TOKEN` | yes | auto-generated | Guards `/pair` endpoint |
| `TARGET_KEYWORDS` | yes | `التسجيل,الجمعة` | Comma-separated, AND-matched |
| `TARGET_GROUP_JID` | after pairing | `120363xxxxx@g.us` | Single group to monitor |
| `PORT` | auto (Render) | `10000` | HTTP bind port |
| `LOG_LEVEL` | no | `INFO` | `DEBUG` / `INFO` / `WARN` / `ERROR` |
| `TZ` | no | `Asia/Riyadh` | Log timestamp timezone |

## Local development

```bash
# 1. Start a local Postgres (or point at your Neon instance)
export DATABASE_URL="postgresql://localhost/sniper?sslmode=disable"
export PAIR_TOKEN="dev-secret"
export TARGET_KEYWORDS="test"
export PORT=10000

# 2. Resolve dependencies & generate go.sum
go mod tidy

# 3. Run
go run ./cmd/sniper

# 4. Pair: open http://localhost:10000/pair?token=dev-secret in your browser
#    and scan the QR with your phone.
```

## Deploy to Render

### One-time setup

1. Create a Neon project and copy the **pooled** connection string.
2. Push this repository to GitHub (private).
3. In Render dashboard: **New +** → **Web Service** → connect the GitHub repo.
4. Render auto-detects `render.yaml` → click **Apply**.
5. In the service's **Environment** tab, fill the two `sync: false` secrets:
   - `DATABASE_URL` → your Neon pooled URL
   - `TARGET_GROUP_JID` → leave empty for now
6. Deploy.

### First pair (one-time)

1. Check Render logs. The `PAIR_TOKEN` value is in the Environment tab.
2. Open `https://<your-service>.onrender.com/pair?token=<PAIR_TOKEN>` in any browser.
3. On your phone: WhatsApp → Settings → Linked Devices → Link a Device → scan.
4. Logs print `[wa] pair success`.
5. From your phone, send `!id` in the target group. Logs print the group JID.
6. Paste it into Render's `TARGET_GROUP_JID` env var → Render redeploys in ~30 s using cached layers.

### Keep-alive

1. Sign up at UptimeRobot (free).
2. New monitor → HTTP(s) → URL = `https://<your-service>.onrender.com/health`
3. Interval: **5 minutes**.

This keeps the service warm so the WhatsApp WebSocket never gets torn down by Render's 15-minute idle policy.

## Owner commands

Sent from your own paired phone in any chat. Prefix `!`:

| Command | Effect |
|---|---|
| `!id` | Replies with the current chat + sender JIDs |
| `!status` | Runtime metrics (uptime, detect/react counts, memory) |
| `!ping` | `pong ⚡` liveness check |
| `!help` | Command list |

## HTTP endpoints

| Path | Auth | Purpose |
|---|---|---|
| `GET /` | none | Plain-text one-liner summary |
| `GET /health` | none | 200 if connected, 503 if disconnected (200 during pairing) |
| `GET /status` | none | JSON metrics snapshot |
| `GET /pair?token=…` | token | HTML page with the active QR code or paired status |

## Operational notes

- **Never delete the `whatsmeow_device` row** in Postgres unless you want to re-pair. Other tables (`whatsmeow_sessions`, `whatsmeow_sender_keys`, `whatsmeow_pre_keys`, `whatsmeow_message_secrets`) are self-healing by protocol and should never be manually truncated.
- If the device gets force-unlinked from WhatsApp (e.g., you hit "Log out from all devices" on your phone), the bot logs `[wa] logged out` and re-enters pairing mode. Visit `/pair` again.
- **Do NOT run two copies** of the bot against the same Neon database simultaneously. WhatsApp enforces a single active session per linked device; the second connection triggers `stream replaced` and both sessions die.
- Postgres storage for a single device is typically <2 MB even after months of operation.

## Legacy files

The original Baileys bot (`baileys-bot.js`, `package.json`) is kept in-tree for reference but is excluded from the Docker build via `.dockerignore`. You can delete them whenever you've verified the Go version.

## License

MIT — author: Shaheed Alkabodi
