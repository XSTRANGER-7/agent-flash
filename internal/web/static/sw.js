// Agent Arena service worker: app shell offline, API network-first.
const VERSION = "arena-v27";
const SHELL = [
  "/", "/index.html", "/styles.css", "/app.js", "/manifest.webmanifest", "/fonts/outfit-latin.woff2",
  "/icons/icon-192.png", "/icons/icon-512.png", "/icons/maskable-512.png",
  "/icons/apple-touch-icon.png", "/icons/favicon-32.png",
];

self.addEventListener("install", (event) => {
  event.waitUntil(caches.open(VERSION).then((c) => c.addAll(SHELL)));
});

self.addEventListener("activate", (event) => {
  event.waitUntil(
    caches.keys().then((keys) => Promise.all(keys.filter((k) => k !== VERSION).map((k) => caches.delete(k))))
      .then(() => self.clients.claim())
  );
});

self.addEventListener("message", (event) => {
  if (event.data === "skip-waiting") self.skipWaiting();
});

self.addEventListener("fetch", (event) => {
  const req = event.request;
  const url = new URL(req.url);
  if (req.method !== "GET" || url.origin !== self.location.origin) return;
  // Never cache the live stream, admin data or metrics.
  // Wallet data is personal; share pages and RSS are for crawlers and readers.
  if (url.pathname.startsWith("/media/")) return; // video: let the browser stream it (range requests)
  if (url.pathname.startsWith("/v1/wallet") || url.pathname.startsWith("/v1/me") || url.pathname.startsWith("/s/") || url.pathname === "/signals.rss") return;
  if (url.pathname.startsWith("/v1/feed/stream") || url.pathname.startsWith("/v1/admin") ||
      url.pathname === "/metrics" || url.pathname === "/readyz" || url.pathname === "/healthz") return;

  if (url.pathname.startsWith("/v1/")) {
    event.respondWith(networkFirst(req));
    return;
  }
  if (req.mode === "navigate") {
    event.respondWith(fetch(req).catch(() => caches.match("/index.html")));
    return;
  }
  event.respondWith(staleWhileRevalidate(req));
});

async function networkFirst(req) {
  const cache = await caches.open(VERSION);
  try {
    const res = await fetch(req);
    if (res.ok) cache.put(req, res.clone());
    return res;
  } catch {
    const hit = await cache.match(req);
    if (hit) return hit;
    return new Response(JSON.stringify({ error: { message: "offline" } }), {
      status: 503, headers: { "Content-Type": "application/json" },
    });
  }
}

async function staleWhileRevalidate(req) {
  const cache = await caches.open(VERSION);
  const hit = await cache.match(req);
  const fresh = fetch(req).then((res) => {
    if (res.ok) cache.put(req, res.clone());
    return res;
  }).catch(() => hit);
  return hit || fresh;
}
