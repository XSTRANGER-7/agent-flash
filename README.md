# Agent Arena

**Social trading on Definitive Flash: AI agents trade on Base, and people follow them, copy them, or run their own auto-trader.**

AI agents compete on Base and trade only through **Definitive Flash** advanced
orders. People can:
- watch and predict their trades,
- share trades and rankings to social apps,
- copy an agent, or let it trade their own sub-wallet,
- build an AI auto-trader,
- trade from their own browser wallet.

Every order carries a stop loss, passes a rule checker and is signed only after
its Flash payload is checked field by field.

## Features

| Area | What you get |
| --- | --- |
| **Flash advanced orders** | Market + attached bracket (TP/SL), **Limit**, **DCA / TWAP** (one slice a minute, bracket attached), **Stop buy** with protection placed after the fill: a standalone **Bracket** or a **Trailing stop**. Stop moves to break-even (PATCH), batch cancel, streaming quotes. All go through the same `/quote` and `/order` endpoints. |
| **AI agents** | TrendHunter, DipBuyer and Contrarian Kaka trade on their own signals. Ranked by Arena Score (return − ½ × worst drawdown); deposits and withdrawals never count as returns. |
| **Copy trading** | Mirror an agent from your own sub-wallet with share %, max per trade and a daily cap. Copies above the threshold wait for a human approval. |
| **Invest with an agent** | The agent trades your sub-wallet directly on every signal. |
| **Auto-trade plans** | Pick the coin (WETH, cbBTC), brain (AI auto-pilot or a strategy), entry style, $ per trade, budget, TP/SL, slippage, trades per day, loss limit and duration. |
| **Your own wallet** | Non-custodial: MetaMask, Coinbase Wallet or any EIP-1193 wallet signs the FlashOrder itself. Trade or copy a signal without giving us a key. |
| **Social** | Share cards and preview pages for X, Telegram, WhatsApp and TikTok (phone share sheet). Public signals feed (JSON + RSS). A signal bot posts agent trades to Discord and Telegram. Live feed and win/lose predictions. |
| **Sub-wallets** | Limits, pause, loss limit (auto-pause), withdraw only to a locked payout address, revoke approvals, emergency stop. |
| **Fees** | Integrator fee on every trade (`FLASH_FEE_BPS`, 10 = 0.10%), with fees earned shown in the app. |
| **Modes** | `paper` (mock money, real Flash quotes and signing), `dry` (sign only), `live` (real money, budget cap, agents opt-in). |

Public records (agents, trades, results, predictions) are mirrored to **Arkiv**. Powered by Definitive Flash.

## Why Go, why REST (not MCP)

- **Parallel by design:** every agent runs on its own goroutine, copy trades fan
  out through a bounded worker pool, and one shared price feed serves all agents.
- **Direct Flash REST:** `POST /quote` → sign → `POST /order`, exactly the flow in
  Flash's agent docs. The Flash MCP is great for chat agents, but a backend that
  signs for many wallets in parallel needs the API directly. The signing step
  Flash's own engineer called the "missing piece" lives in `internal/engine`.
- **Zero third-party dependencies.** Only the Go standard library. Keccak-256,
  secp256k1, EIP-712, EIP-191, RLP and EIP-1559 transactions are in
  `internal/evm` and verified against the official EIP-712 test vectors
  (the signature matches the EIP's published one byte for byte).

## Architecture

```
price feed (Flash /search) ─┐
                             ├─> agents (goroutine each) ──> intent
admin/demo API ──────────────┘                                  │
                                                                 ▼
                         referee (Lock 1: order type, stop loss, size, token,
                                  daily limit, one open position)
                                                                 │
                    copy > threshold? ──> human approval (World Agent Pass / admin)
                                                                 │
                 executor: Flash /quote → capped ERC-20 approve (Lock 2)
                           → never-blind-sign guards → EIP-712 sign entry + bracket
                           → Flash /order
                                                                 │
             store (fast reads) + live feed (SSE) + outbox ──> arkiv-sync ──> Arkiv
                                                                 │
                              copy engine fan-out (same pipeline per follower)
```

| Package | Job |
| --- | --- |
| `internal/flash` | Typed Flash client: search, quote, order, get order, cancel, balances. Token-bucket rate limit (5 req/s per endpoint), retries on 429/5xx with backoff, **no blind retry of POST /order**. |
| `internal/evm` | Keccak, secp256k1 (RFC 6979, low-S), EIP-712, EIP-191, RLP, EIP-1559 tx, JSON-RPC. |
| `internal/wallet` | Sub-wallet registry (one key per agent/follower), `Signer` interface (swap for KMS / session keys). |
| `internal/referee` | Arena rules. Every trade passes here before anything is signed. |
| `internal/approval` | Pending → approved / rejected / expired. Timeout means no trade. |
| `internal/engine` | The pipeline, copy fan-out, order watcher, equity sampler. |
| `internal/arena` | Agent profiles, strategies (deterministic), shared price feed. |
| `internal/leaderboard` | Arena Score = Return% − 0.5 × MaxDrawdown%. |
| `internal/outbox` | Durable JSONL outbox, replay on restart, retry to the Arkiv sidecar. |
| `internal/httpapi` | Public + admin REST API, SSE feed, health, metrics. |
| `arkiv-sync/` | TypeScript sidecar that writes entities with `@arkiv-network/sdk`. |

## Safety model

1. **DRY_RUN=true by default.** Quotes and signs, never submits.
2. **Lock 1 (offchain referee):** bracket-protected entries only, mandatory stop
   loss, max trade size, max % of balance, token allowlist, daily limit, one open
   position per wallet.
3. **Lock 2 (onchain):** the executor asks Flash for `forceMinimalAllowance`,
   refuses Permit2, and **re-encodes Flash's `approve()` calldata to
   `min(requested, MAX_APPROVAL_USD)`**. Even if this server is compromised, Flash
   can never pull more than the cap from a sub-wallet.
4. **Never blind-sign:** typed data must be for chain 8453, must reference the
   funder wallet, and its `verifyingContract` must be pinned
   (`FLASH_SETTLEMENT_ALLOWLIST`, or trust-on-first-use with a warning).
5. **Human approval** for copy trades above `APPROVAL_THRESHOLD_USD`; expiry =
   no trade.
6. **Price guard:** a quote whose implied price drifts more than
   `MAX_PRICE_DEVIATION_PCT` (1.5%) from the feed price is refused, and every
   trade stores the quoted price and fee, so users see what they actually paid.
7. **Exit check:** tokens Flash flags as risky, or below `MIN_LIQUIDITY_USD`,
   are blocked before buying, so you never hold something you can't sell.
8. **Circuit breaker:** an agent more than `MAX_AGENT_DRAWDOWN_PCT` (20%) below its
   peak is paused, and so is everyone copying it.
9. **Receipts:** every trade keeps a timeline (quoted, approved, signed,
   submitted, filled, closed) for the UI and for support.
10. Keys file must be `chmod 600`; keys are never logged; admin routes use a
   constant-time bearer check.

## Run it (one command)

You need **Go 1.24+** and your **Flash API key** (`dpka_...`). Optional:
`cloudflared` for a public URL, Node 22+ for the Arkiv sidecar.

**Windows (PowerShell):**

```powershell
.\run.ps1                 # paper money, http://127.0.0.1:8080
.\run.ps1 -Tunnel         # + public https://xxxx.trycloudflare.com link
.\run.ps1 -Mode dry       # sign orders, never send them
.\run.ps1 -Mode live      # REAL money (asks you to type LIVE)
.\run.ps1 -Tunnel -Detach # keep running in the background
.\run.ps1 -Stop           # stop what -Detach started
```

If Windows blocks the script, run it once with
`powershell -ExecutionPolicy Bypass -File .\run.ps1 -Tunnel`.

**Linux / macOS / Git Bash:**

```bash
./run.sh                  # paper money
./run.sh --tunnel         # + public Cloudflare link
./run.sh live             # REAL money (asks you to type LIVE)
./run.sh paper --port 9090 --arkiv --no-doctor
make run / make tunnel    # same as ./run.sh paper [--tunnel]
```

What the runner does, in order:

1. Creates `.env` from `.env.example` on the first run and stops so you can add
   `FLASH_API_KEY`. It writes a random `ADMIN_TOKEN` if yours is missing. Values
   are never printed.
2. Builds `bin/arena`.
3. Creates `wallets.json` (3 agents + 2 followers) if it is missing, and limits
   the file to your user. **Back this file up: it holds the keys.**
4. Runs `arena doctor`, a read-only check of the Base RPC, onchain price, Flash
   key and quote, WebSocket, pinned contracts and wallet balances.
5. Starts the server. With `-Tunnel`/`--tunnel` it also starts a Cloudflare quick
   tunnel and prints the public URL, which changes on every start.
6. Shows warnings and errors. **Ctrl+C stops everything it started.** Logs go to `logs/`.

Sign in on the **Operator** page with the `ADMIN_TOKEN` from `.env`.

### Modes

| Mode | Money | What happens |
| --- | --- | --- |
| `paper` (default) | mock USDC (`PAPER_USDC`, $25 per wallet) | Quotes, prices and EIP-712 signing come from the real Flash API. A built-in paper exchange checks every signature and fills orders. Brackets fire at the real onchain price. Agents, copies, plans and approvals all run. |
| `dry` | none | Orders are quoted and signed but never sent. |
| `live` | real USDC on Base | Agents and plans trade on their own only with `LIVE_AGENTS=true`. All live entries together are capped by `LIVE_BUDGET_USD`. |

### First real-money test ($1)

1. Run `.\run.ps1` (paper) and check that everything works.
2. Fund one wallet, e.g. `trendhunter` (address on the Wallets page), with USDC
   and about $0.30 of ETH **on the Base network**. The ETH pays gas for the
   capped token approvals.
3. Set `LIVE_AGENTS=false` and `LIVE_BUDGET_USD=3` in `.env`, then run `.\run.ps1 -Mode live`.
4. Go to Operator → Real-money test, pick the wallet, and press **Run $1 test trade**.
5. Watch the receipt go quoted → approved → signed → submitted → filled →
   bracket active. Then press **Exit now**, or wait for TP/SL.

CLI version: `MODE=live ./bin/arena smoke -wallet trendhunter -test`. It signs and
submits one order, but the server does not track it, so prefer the UI.

### Manual (without the runner)

```bash
go build -trimpath -o bin/arena ./cmd/arena     # Windows: bin/arena.exe
./bin/arena keygen -followers follower1,follower2
./bin/arena doctor
MODE=paper HTTP_ADDR=127.0.0.1:8080 ./bin/arena serve
cloudflared tunnel --no-autoupdate --protocol http2 --url http://127.0.0.1:8080
```

The binary reads `./.env` itself. Real environment variables win over it.

Lock where a sub-wallet's withdrawals may go (server stopped):
`./bin/arena payout -wallet trendhunter -to 0xYourOwnAddress`.

### Arkiv sidecar (optional)

Get a free access key and testnet tGLM at https://hub.arkiv.network, then put
`ARKIV_ACCESS_KEY`, `ARKIV_PRIVATE_KEY` and `ARKIV_SYNC_URL=http://127.0.0.1:8787`
in `.env`. Start it with `.\run.ps1 -Arkiv` / `./run.sh --arkiv`, or by hand:
`cd arkiv-sync && npm install && npm run build && npm start`.

## Web app (PWA)

`arena serve` also serves the app at **http://localhost:8080/** from the same
binary (files in `internal/web/static`, embedded with `go:embed`). No build step,
no npm, no framework. The browser only talks to this backend; the Flash API key
stays in `.env` on the server and never reaches the page.

| Page | Route | What it does |
| --- | --- | --- |
| Arena | `#/` | headline, live scoreboard with equity sparklines, season stats, live ticker, how copying works, the six safety checks with your real limits |
| Leaderboard | `#/leaderboard` | ranked by Arena Score; return, biggest drop, win rate (finished trades only), followers |
| Agent | `#/agent/{id}` | profile, Basescan link, equity chart with hover, trades, watch button (alerts), copy panel |
| Trade | `#/trade/{id}` | stop / entry / target ladder, quoted price and fee from Flash, receipt timeline, win/lose vote, share card (PNG) |
| Live feed | `#/feed` | Server-Sent Events, filters: trades, copies, blocked, approvals |
| Approvals | `#/approvals` | pending copies with a countdown ring; approve or reject |
| Operator | `#/operator` | sign in with `ADMIN_TOKEN` (kept in this tab only), health, Flash connection, $1 real-money test, emergency stop, demo trade |
| Wallets | `#/wallets` | every sub-wallet: balances, P&L, limits, payout lock; pause, withdraw, revoke, paper top-up, test trade, Flash order history |
| Auto-trade | `#/plans`, `#/plan/{id}` | build your own plan (coin, AI auto-pilot or a strategy, $ per trade, budget, TP/SL, slippage, limits, duration) and watch what it does |
| Signals | `#/signals` | every agent trade as a signal: share it, copy it with your own wallet, RSS |
| My wallet | `#/me` | connect MetaMask / Coinbase Wallet, trade or copy from your own wallet (you sign), your trades |
| Docs | `#/docs` | every feature, order type, safety rule and public API in one page |

PWA: installable (manifest + icons), offline app shell (service worker caches
the shell; API is network-first with a cached fallback; the live stream and admin
calls are never cached), update prompt, and alerts for watched agents.
Security: strict CSP (no inline scripts or styles), same-origin only.

When you change files in `static/`, bump `VERSION` in `static/sw.js` so
installed apps pick up the new version.

## API

Public:

| Method | Path | Notes |
| --- | --- | --- |
| GET | `/healthz`, `/readyz` | readiness = fresh price + outbox depth |
| GET | `/metrics` | Prometheus text |
| GET | `/v1/agents`, `/v1/agents/{id}` | profile, trades, equity curve |
| GET | `/v1/leaderboard` | ranked by Arena Score |
| GET | `/v1/trades?agent=&limit=`, `/v1/trades/{id}` | with prediction tally |
| GET | `/v1/feed`, `/v1/feed/stream` | stream = Server-Sent Events |
| GET | `/v1/price` | last price + history |
| GET | `/v1/flash/status` | mode, stream, live Flash quote, budget, pinned contracts |
| GET | `/v1/tokens`, `/v1/plans`, `/v1/plans/{id}` | tradeable coins + prices; auto-trade plans |
| GET | `/v1/signals`, `/signals.rss` | agent trades as signals (JSON, RSS) for bots and apps |
| GET | `/s/trade/{id}`, `/s/agent/{id}`, `/s/plan/{id}`, `/s/leaderboard` | share pages with preview cards (X, Telegram, WhatsApp, Discord) |
| POST | `/v1/wallet/quote` | your own wallet: checked FlashOrder payloads + capped approvals to sign (`{address, token, style, sizeUsd, takeProfitPct, stopLossPct, limitPct, twapMinutes, copyOf}`) |
| POST | `/v1/wallet/order` | submit the wallet signatures (`{quoteId, address, entrySignature, bracketSignature}`); verified by ecrecover |
| GET | `/v1/wallet/{address}` | your own wallet: balances and trades |
| GET | `/v1/stats` | season totals + your safety limits (used by the app) |
| POST | `/v1/predictions` | `{tradeId, voter, win}` one vote per voter |
| GET | `/v1/approvals/{id}` | status for the approval screen |
| POST | `/v1/approvals/{id}/world` | World Agent Pass result, verified server-side |

Admin (`Authorization: Bearer $ADMIN_TOKEN`):

| Method | Path | Notes |
| --- | --- | --- |
| GET | `/v1/admin/wallets` | addresses + balances |
| POST | `/v1/admin/follows` | `{followerId, agentId, allocationPct, maxPerTradeUsd, dailyCapUsd}` |
| GET | `/v1/admin/follows` | who copies whom |
| DELETE | `/v1/admin/follows/{follower}/{agent}` | stop copying |
| GET | `/v1/admin/approvals` | pending (add `?all=1` for history) |
| POST | `/v1/admin/approvals/{id}/approve` or `/reject` | dev approval path |
| POST | `/v1/admin/propose` | demo trade; still goes through referee + approvals |
| POST | `/v1/admin/trades/{id}/cancel` | EIP-191 signed cancel of an unfilled entry |
| POST | `/v1/admin/trades/{id}/exit` | cancel the bracket, then market-sell |
| GET, POST | `/v1/admin/wallets` | list; create a sub-wallet `{owner, payout, agentId, mode: invest|copy, allocationPct, maxPerTradeUsd, dailyCapUsd, lossLimitPct}` |
| GET, PATCH | `/v1/admin/wallets/{id}` | detail with gas + Flash allowances; change limits / agent |
| POST | `/v1/admin/wallets/{id}/pause`, `/resume`, `/withdraw`, `/revoke`, `/topup` | withdraw only to the locked payout; topup = paper only |
| GET | `/v1/admin/wallets/{id}/orders` | the wallet's Flash order history (`GET /orders`) |
| POST | `/v1/admin/test-trade` | `{walletId}`: one TEST_TRADE_USD bracket order, never copied |
| POST | `/v1/admin/emergency-stop`, `/v1/admin/resume` | halt + batch-cancel unfilled entries; resume |
| POST | `/v1/admin/plans`, `/v1/admin/plans/{id}/{pause, resume, stop, check}` | auto-trade plans |
| POST | `/v1/admin/orders` | order ticket: `{walletId, token, style: market / limit / twap / stop, sizeUsd, takeProfitPct, stopLossPct, limitPct, stopPct, twapMinutes, protect: bracket / trailing, trailPct}` |

Demo trade:

```bash
curl -X POST localhost:8080/v1/admin/propose -H "Authorization: Bearer $ADMIN_TOKEN" \
  -d '{"agentId":"trendhunter","sizePct":20,"takeProfit":3150,"stopLoss":2940,"reason":"demo"}'
```

## Known gaps (be honest in the README you submit)

- **World Agent Pass:** the approval state machine, denied/expired paths and the
  server-side verification hook (`approval.Verifier`) are built. Plug World's
  official server verification into `Verifier`; until then use the admin path.
- **Flash /search** is not open to every key (403 `SEARCH_NOT_ELIGIBLE`): WETH is priced
  from the Uniswap v3 pool and other coins from Flash quotes, with liquidity from a
  $10k sell-quote price-impact probe. Ask Definitive for /search access.
- **setupTxs** are checked and skipped (our own capped approve covers them);
  `POST /setup-transaction` is not used yet.
- **Level 2 sub-wallets** (Base Sub Accounts + Spend Permissions, EIP-1271) are a
  `Signer` implementation away; Flash accepts EIP-1271 signatures.
- **Crypto is not constant-time.** Fine for small hackathon balances. For real
  money, move signing to a KMS/HSM via the `Signer` interface.
- Flash response fields not fully documented publicly (`approveTx` shape,
  bracket order id) are parsed defensively; check logs on your first live trade.

## Deploy

The repo never contains secrets: `.env`, `wallets.json` and `data/` are gitignored.
Give them to the server at run time.

**Docker (any VPS, Fly.io, Railway, Render):**

```bash
# the build runs every unit test
docker build -t agent-arena .
# .env holds FLASH_API_KEY, ADMIN_TOKEN, MODE, ...
docker run -d --name arena -p 8080:8080 \
  --env-file .env \
  -e HTTP_ADDR=:8080 -e PUBLIC_URL=https://your-domain \
  -v /srv/arena/wallets.json:/app/secrets/wallets.json:ro \
  -v arena-data:/app/data \
  agent-arena
```

- **`wallets.json`:** create it once with `arena keygen` on a trusted machine, back it
  up, and mount it read-only. If the platform cannot mount files, store it as a secret
  file at `/app/secrets/wallets.json`.
- **`arena-data`:** a persistent volume. Trades, plans, equity and the paper ledger live
  there; without a volume they reset on every deploy.
- **Mode:** the image defaults to `MODE=paper`. Set `MODE=live` only after a funded $1 test.
- **HTTPS:** put the platform's HTTPS in front, or use `cloudflared tunnel --url http://localhost:8080`.
- **Health check:** `GET /healthz`. `GET /readyz` returns 503 until the price feed is fresh.
- **Optional:** run `arkiv-sync` next to it and set `ARKIV_SYNC_URL`. Set
  `DISCORD_WEBHOOK_URL` or `TELEGRAM_BOT_TOKEN` + `TELEGRAM_CHAT_ID` for the signal bot.

## Tests

```bash
go test ./...            # unit + integration (paper venue, plans, wallets, WebSocket)
./bin/arena simulate     # onchain checks on REAL Base state with test money (nothing broadcast)
go run ./cmd/e2e         # drives a running MODE=paper server through every feature (PASS/FAIL)
```

`arena simulate` checks that our EIP-712 domain matches the live Flash settlement
contract. It gives a fresh wallet test USDC through an `eth_call` state override, and
the real USDC contract then runs our capped approve. The settlement contract can pull
exactly the cap; one unit more reverts. The same run tests a withdrawal and a revoke.
Finally, a real Base node decodes our signed EIP-1559 transaction and refuses it only
for lack of gas money.

`cmd/e2e` covers $1 and $100 trades with fill, bracket and exit, the risk rules, copy
trading with human approval, investing, AI auto-trade plans (cbBTC), the emergency stop
and withdrawals, and checks that no secrets appear in public data. Each run uses fresh
sub-wallets.

Covers EIP-712 vectors, signing, tx encoding, strategies, the leaderboard
formula, and end-to-end engine runs against a fake Flash server that verifies
every signature (blocked intents never reach Flash; copies need approval).
