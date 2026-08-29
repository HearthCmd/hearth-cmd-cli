// Hearth display kiosk service worker.
//
// Goal: a screen that boots without network still loads the UI (which then shows
// "Reconnecting…" until /ws/screen returns). Vite emits hash-named assets, so a
// precache manifest would go stale every build — instead we cache at runtime:
// serve from network, fall back to cache, and stash successful same-origin GETs.
// The websocket and any /ws path are never touched.
const CACHE = 'hearth-kiosk-v1'

self.addEventListener('install', () => {
  self.skipWaiting()
})

self.addEventListener('activate', (event) => {
  event.waitUntil(self.clients.claim())
})

self.addEventListener('fetch', (event) => {
  const req = event.request
  const url = new URL(req.url)
  if (req.method !== 'GET' || url.origin !== location.origin || url.pathname.startsWith('/ws')) {
    return
  }
  event.respondWith(
    fetch(req)
      .then((res) => {
        if (res && res.ok) {
          const copy = res.clone()
          caches.open(CACHE).then((c) => c.put(req, copy)).catch(() => {})
        }
        return res
      })
      .catch(() =>
        caches.match(req).then((hit) => hit || caches.match('/').then((root) => root || Response.error())),
      ),
  )
})
