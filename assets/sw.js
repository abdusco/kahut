// Existing tabs keep their current worker until they close. This prevents a new
// worker from serving a different shell version beneath an already-running app.
const CACHE = 'kahut-shell-v24';
const SHELL = [
  '/',
  '/style.css',
  '/boot.js',
  '/app.js',
  '/live.js',
  '/routes.js',
  '/i18n.js',
  '/vendor/alpine-3.15.0.js',
  '/vendor/qrcode-1.4.4.js',
  '/manifest.webmanifest',
  '/icons/icon.svg',
  '/icons/hat.svg',
  '/icons/maskable.svg',
  '/icons/icon-192.png',
  '/icons/icon-512.png',
  '/icons/maskable-512.png',
];
self.addEventListener('install', (event) => {
  event.waitUntil(caches.open(CACHE).then((cache) => cache.addAll(SHELL)));
});
self.addEventListener('activate', (event) => {
  event.waitUntil(
    (async () => {
      for (const key of await caches.keys()) {
        if (key.startsWith('kahut-shell-') && key !== CACHE) await caches.delete(key);
      }
      await self.clients.claim();
    })(),
  );
});
self.addEventListener('fetch', (event) => {
  const { request } = event;
  const url = new URL(request.url);
  if (
    request.method !== 'GET' ||
    url.origin !== self.location.origin ||
    url.pathname.startsWith('/api/')
  )
    return;
  const isPage =
    /^\/(host(\/quizzes\/(new|[A-Za-z0-9_-]+\/edit)|\/games\/[A-Za-z0-9]{8})?|rooms\/[A-Za-z0-9]{8}|resume)?\/?$/.test(
      url.pathname,
    );
  const path = request.mode === 'navigate' && isPage ? '/' : url.pathname;
  if (!SHELL.includes(path)) return;
  event.respondWith(
    caches.open(CACHE).then(async (cache) => (await cache.match(path)) || fetch(request)),
  );
});
