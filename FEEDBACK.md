# Uniswap developer feedback

Agent Arena uses the Uniswap v3 WETH/USDC pool on Base
([`0xd0b53D9277642d899DF5C87A3966A349A798F224`](https://basescan.org/address/0xd0b53D9277642d899DF5C87A3966A349A798F224))
as its independent onchain price: a Go backend that calls the pool directly with
`eth_call` (no SDK, no API key, no subgraph).

## What we use, and where

| Pool call | What it does in Agent Arena | Code |
|---|---|---|
| `token0()`, `token1()`, `decimals()` | Checks at startup that the pool really pairs WETH with USDC and in which order | `internal/arena/pool.go` (`Verify`) |
| `slot0()` | Live reference price. Every Definitive Flash quote is compared with it before an order is signed (price guard) | `internal/arena/pool.go` (`Price`), `internal/engine/executor.go` (`checkPrice`) |
| `balanceOf(pool)` on both tokens | Pool TVL as exit liquidity, so agents never trade a coin they could not sell | `internal/arena/pool.go` (`TVL`) |
| `observe(uint32[])` | About 4 hours of time-weighted prices at 15-second steps from one call. Used to backtest agent rules before launch and to fill the price record behind the live trade charts | `internal/arena/pool.go` (`History`), `internal/engine/backtest.go`, `internal/arena/backtest.go` |

## What worked well

- One `observe()` call returned 1,000 time-weighted prices (4.2 hours) in about 0.3 s from the public Base RPC. That is a trustless price history with no indexer.
- Time-weighted averages per 15-second step ignore a single odd trade inside a block, which makes backtests and the price guard sturdier than `slot0` alone.
- The tick math is simple once the token order and decimals are known: `price = 1.0001^avgTick × 10^(decWETH − decUSDC)`.

## What was hard

1. **No official Go library.** We hand-encoded the `observe(uint32[])` call (dynamic array ABI) and decoded `int56` tick cumulatives sign-extended to 256 bits. A small reference implementation outside JS/TS would save time.
2. **Knowing how far back you can ask.** `observe` reverts (`OLD`) when `secondsAgo` is older than the oldest stored observation. We read `observationCardinality` from `slot0`, but the time span still depends on how often the pool trades, so we retry with fewer points until it succeeds. A view that returns the oldest observation timestamp would remove the guessing.
3. **Token order and decimals.** WETH/USDC is token0/token1 on this pool, but the reverse on others. Getting it wrong silently inverts the price, so we verify `token0`/`token1` at startup and refuse to run otherwise.
4. **`slot0` can be moved inside a block.** We use it only as a guard (with a maximum allowed deviation) and use `observe` averages for history.

## Requests

- A documented helper (or periphery view) that returns the oldest observation timestamp and the cardinality in one call.
- Go (and Python) examples for `observe`, alongside the TypeScript ones.
- Guidance on the observation cardinality recommended for pools that apps use as oracles on L2s.
