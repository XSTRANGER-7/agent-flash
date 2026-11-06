// arkiv-sync: receives public events from the Go backend's outbox and writes
// them to Arkiv (Rabdi testnet) as entities, using @arkiv-network/sdk 1.x.
//
// Why a sidecar: Arkiv ships a TypeScript SDK; keeping it here keeps the Go
// trading core dependency-free, and the outbox guarantees no event is lost
// while this process is down (it retries with backoff).
//
// Needs (all free, from https://hub.arkiv.network):
//   ARKIV_ACCESS_KEY   access key (the public RPC is heavily rate limited)
//   ARKIV_PRIVATE_KEY  0x key of an account funded with testnet tGLM (faucet)
//   ARKIV_SYNC_TOKEN   shared secret, same value as the Go backend's
import http from "node:http";
import { createPublicClient, createWalletClient, ExpirationTime } from "@arkiv-network/sdk";
import { rabdi } from "@arkiv-network/sdk/chains";
import { and, eq } from "@arkiv-network/sdk/query";
import { bool, bytes, dec, defineSchema, str } from "@arkiv-network/sdk/schema";
import { http as transport, isHex, stringToBytes } from "viem";
import { privateKeyToAccount } from "viem/accounts";

const PORT = Number(process.env.ARKIV_SYNC_PORT ?? process.env.PORT ?? 8787);
const TOKEN = process.env.ARKIV_SYNC_TOKEN ?? "";
const PROJECT = process.env.ARKIV_PROJECT ?? "agent-arena";
const KEY = process.env.ARKIV_PRIVATE_KEY;
if (!KEY || !isHex(KEY)) {
  throw new Error("ARKIV_PRIVATE_KEY must be the 0x private key of an account funded at https://hub.arkiv.network/faucet");
}
if (!process.env.ARKIV_ACCESS_KEY) {
  console.warn(JSON.stringify({ msg: "ARKIV_ACCESS_KEY not set: the public Rabdi RPC is heavily rate limited (get a free key at hub.arkiv.network/access-keys)" }));
}

// One schema for every public record. Indexed columns can be filtered on;
// everything else (the full record) is in `payload` as JSON bytes.
const ArenaEvents = defineSchema("arenaevents", {
  project: str().indexed(),
  eventid: str().indexed(),
  kind: str().indexed(), // agent | trade | trade_result | prediction
  season: str().indexed().optional(),
  agentid: str().indexed().optional(),
  tradeid: str().indexed().optional(),
  wallet: str().indexed().optional(),
  status: str().indexed().optional(),
  token: str().optional(),
  strategy: str().optional(),
  sizeusd: dec().optional(),
  copy: bool().optional(),
  payload: bytes(),
});

const chain = rabdi({ accessKey: process.env.ARKIV_ACCESS_KEY ?? "" });
const account = privateKeyToAccount(KEY);
const wallet = createWalletClient({ chain, transport: transport(), account });
const reader = createPublicClient({ chain, transport: transport() });

type Event = {
  id: string;
  kind: string;
  attributes?: Record<string, unknown>;
  payload?: unknown;
  ttlHours?: number;
};

const s = (v: unknown) => (typeof v === "string" && v !== "" ? v : typeof v === "number" ? String(v) : undefined);

async function alreadyWritten(id: string): Promise<boolean> {
  const res = await reader
    .select({ $key: true })
    .from(ArenaEvents)
    .where(and(eq("project", PROJECT), eq("eventid", id)))
    .ownedBy(account.address)
    .limit(1)
    .fetch();
  return res.entities.length > 0;
}

async function write(e: Event) {
  if (!e?.id || !e?.kind) throw new Error("event needs id and kind");
  if (await alreadyWritten(e.id)) return { skipped: true };
  const a = e.attributes ?? {};
  const size = typeof a.sizeusd === "number" ? a.sizeusd : undefined;
  const { entityKey, txHash } = await wallet.createEntity(
    ArenaEvents,
    {
      project: PROJECT,
      eventid: e.id,
      kind: e.kind,
      season: s(a.season),
      agentid: s(a.agentid),
      tradeid: s(a.tradeid),
      wallet: s(a.wallet),
      status: s(a.status),
      token: s(a.token),
      strategy: s(a.strategy),
      sizeusd: size === undefined ? undefined : dec(size.toFixed(6)),
      copy: a.copy === undefined ? undefined : a.copy === 1 || a.copy === true,
      payload: stringToBytes(JSON.stringify({ attributes: a, payload: e.payload ?? null })),
    },
    { expiration: ExpirationTime.fromHours(Math.max(1, e.ttlHours || 24)) },
  );
  console.log(JSON.stringify({ msg: "entity written", id: e.id, kind: e.kind, entityKey, txHash }));
  return { entityKey, txHash };
}

const server = http.createServer(async (req, res) => {
  if (req.method === "GET" && req.url === "/healthz") {
    res.writeHead(200, { "content-type": "application/json" }).end(JSON.stringify({ ok: true, owner: account.address, chainId: chain.id }));
    return;
  }
  if (req.method !== "POST" || req.url !== "/events") {
    res.writeHead(404).end();
    return;
  }
  if (TOKEN && req.headers.authorization !== `Bearer ${TOKEN}`) {
    res.writeHead(401).end();
    return;
  }
  let body = "";
  for await (const chunk of req) {
    body += chunk;
    if (body.length > 1_000_000) {
      res.writeHead(413).end();
      return;
    }
  }
  try {
    const out = await write(JSON.parse(body) as Event);
    res.writeHead(200, { "content-type": "application/json" }).end(JSON.stringify(out));
  } catch (err) {
    console.error(JSON.stringify({ msg: "write failed", err: String(err) }));
    res.writeHead(502).end(); // the Go outbox retries with backoff
  }
});

// Localhost only: the Go backend is the only caller.
server.listen(PORT, "127.0.0.1", () =>
  console.log(JSON.stringify({ msg: "arkiv-sync listening", port: PORT, owner: account.address, chainId: chain.id })),
);
