// MLDojo service worker: caches the app shell and static assets only.
// Never touches /api/ (REST, WebSockets, artifact bytes) or cross-origin requests.
const CACHE = "mldojo-shell-v1";
const SHELL = ["/", "/manifest.json", "/icons/icon.svg", "/icons/icon-192.png", "/icons/icon-512.png"];

self.addEventListener("install", (event) => {
  event.waitUntil(
    caches
      .open(CACHE)
      .then((c) => c.addAll(SHELL))
      .then(() => self.skipWaiting()),
  );
});

self.addEventListener("activate", (event) => {
  event.waitUntil(
    caches
      .keys()
      .then((keys) => Promise.all(keys.filter((k) => k !== CACHE).map((k) => caches.delete(k))))
      .then(() => self.clients.claim()),
  );
});

function cacheable(res) {
  return res && res.ok && res.type === "basic";
}

self.addEventListener("fetch", (event) => {
  const req = event.request;
  if (req.method !== "GET") return;
  const url = new URL(req.url);
  if (url.origin !== self.location.origin) return; // e.g. NEXT_PUBLIC_API_BASE on another host
  if (url.pathname.startsWith("/api/")) return; // never cache the API
  if (url.searchParams.has("token")) return;

  // Content-hashed build assets: cache-first.
  if (url.pathname.startsWith("/_next/static/") || url.pathname.startsWith("/icons/")) {
    event.respondWith(
      caches.match(req).then(
        (hit) =>
          hit ||
          fetch(req).then((res) => {
            if (cacheable(res)) {
              const copy = res.clone();
              caches.open(CACHE).then((c) => c.put(req, copy));
            }
            return res;
          }),
      ),
    );
    return;
  }

  // Pages and everything else same-origin: network-first, fall back to cache
  // (pages are cached without their query string: /run?id=x -> /run).
  const key = req.mode === "navigate" ? url.origin + url.pathname : req;
  event.respondWith(
    fetch(req)
      .then((res) => {
        if (cacheable(res)) {
          const copy = res.clone();
          caches.open(CACHE).then((c) => c.put(key, copy));
        }
        return res;
      })
      .catch(() =>
        caches.match(key).then((hit) => hit || (req.mode === "navigate" ? caches.match("/") : undefined)).then(
          (hit) => hit || new Response("Offline", { status: 503, statusText: "Offline" }),
        ),
      ),
  );
});
