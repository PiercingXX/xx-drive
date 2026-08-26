/* xx-drive service worker: app-shell caching only.
   API calls are never cached (always live).

   Cache rotation protocol: bump CACHE to 'xxdrive-shell-v<N+1>' on EVERY
   deploy that changes any JS/CSS asset. The activate handler deletes
   caches from old versions, and the stale-while-revalidate fetch handler
   then repopulates from network — without a bump, a cached shell can keep
   serving an old /app.js that 404s against new server routes. */
const CACHE = 'xxdrive-shell-v2';
const SHELL = ['/', '/index.html', '/manifest.webmanifest', '/app.js', '/style.css',
  '/assets/icon.svg'];

self.addEventListener('install', (e) => {
  e.waitUntil(caches.open(CACHE).then((c) => c.addAll(SHELL)).then(() => self.skipWaiting()));
});

self.addEventListener('activate', (e) => {
  e.waitUntil(
    caches.keys().then((keys) =>
      Promise.all(keys.filter((k) => k !== CACHE).map((k) => caches.delete(k)))
    ).then(() => self.clients.claim())
  );
});

self.addEventListener('fetch', (e) => {
  const url = new URL(e.request.url);
  if (e.request.method !== 'GET' || url.pathname.startsWith('/api/') || url.pathname.startsWith('/s/')) {
    return; // network only
  }
  // stale-while-revalidate for the app shell
  e.respondWith(
    caches.match(e.request).then((cached) => {
      const fresh = fetch(e.request).then((resp) => {
        if (resp.ok) {
          const copy = resp.clone();
          caches.open(CACHE).then((c) => c.put(e.request, copy));
        }
        return resp;
      }).catch(() => cached);
      return cached || fresh;
    })
  );
});
