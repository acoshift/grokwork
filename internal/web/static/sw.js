/* Grok Work — minimal service worker for installability (Add to Home Screen).
   Live admin data always goes to the network; no offline shell.

   Non-GET is not intercepted. A pass-through respondWith(fetch(event.request))
   drops POST bodies on WebKit, and iOS Safari 26.5+ already sends an empty
   body for a disk-backed file while any service worker controls the page
   (WebKit 319985). Leaving the event unhandled lets the browser send it. */
self.addEventListener("install", (event) => {
  event.waitUntil(self.skipWaiting());
});

self.addEventListener("activate", (event) => {
  event.waitUntil(self.clients.claim());
});

self.addEventListener("fetch", (event) => {
  if (event.request.method !== "GET" && event.request.method !== "HEAD") {
    return;
  }
  event.respondWith(fetch(event.request));
});
