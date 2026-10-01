/* Timeblaster service worker.
 *
 * Its only job is making the app shell load instantly and survive a moment of
 * Wi-Fi trouble. API responses are never cached: a cached alarm list is worse
 * than no alarm list, because it would show the user something that is not true
 * about their alarm clock.
 */
// Bumped when the shell list changes, so activate() can drop the old cache.
var CACHE = 'timeblaster-v2';
// addAll is all or nothing: one missing entry rejects the whole install and the
// worker never activates, so every path here must exist.
var SHELL = [
  './',
  'index.html',
  'app.css',
  'app.js',
  'manifest.webmanifest',
  'assets/icon-192.png'
];

self.addEventListener('install', function (e) {
  e.waitUntil(
    caches.open(CACHE).then(function (c) { return c.addAll(SHELL); })
      .then(function () { return self.skipWaiting(); })
  );
});

self.addEventListener('activate', function (e) {
  e.waitUntil(
    caches.keys().then(function (keys) {
      return Promise.all(keys.filter(function (k) { return k !== CACHE; })
        .map(function (k) { return caches.delete(k); }));
    }).then(function () { return self.clients.claim(); })
  );
});

self.addEventListener('fetch', function (e) {
  var url = new URL(e.request.url);

  // Never cache the API or the WebSocket.
  if (url.pathname.indexOf('/api/') === 0 || e.request.method !== 'GET') return;

  // Network first, falling back to the cached shell, so a running device always
  // serves the current app and a brief outage still opens something.
  e.respondWith(
    fetch(e.request).then(function (resp) {
      if (resp && resp.ok && resp.type === 'basic') {
        var copy = resp.clone();
        caches.open(CACHE).then(function (c) { c.put(e.request, copy); });
      }
      return resp;
    }).catch(function () {
      return caches.match(e.request).then(function (hit) {
        return hit || caches.match('index.html');
      });
    })
  );
});
