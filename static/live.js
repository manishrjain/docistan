// Keeps pages current while background work finishes.
//
// Ingestion and tagging happen after the response is sent, so a page can be
// correct when rendered and stale a second later. Rather than making the app
// client-rendered for this, the server keeps rendering HTML and this swaps in
// the freshly rendered <main> when something has actually changed.
//
// The document page is excluded: doc.js updates it field by field, which is
// better than replacing markup someone may be editing.

document.addEventListener("DOMContentLoaded", () => {
  const main = document.querySelector("main");
  if (!main) return;
  if (/^\/doc\/\d+$/.test(location.pathname)) return;

  const onStatus = location.pathname === "/status";
  const onIndex = location.pathname === "/";
  if (!onStatus && !onIndex) return;

  // The index only needs watching while something is mid-flight; the status
  // page is about in-flight work by definition.
  const workPending = () =>
    onStatus || !!main.querySelector(".status.processing");

  let idleRounds = 0;

  async function tick() {
    if (!workPending()) {
      // Stop polling once the index has settled, rather than hitting the
      // server forever on a page nobody is watching change.
      if (++idleRounds > 3) return;
      return schedule(5000);
    }
    idleRounds = 0;

    // Never redraw under someone's cursor.
    if (document.activeElement && main.contains(document.activeElement)) {
      return schedule(3000);
    }
    if (document.hidden) return schedule(5000);

    try {
      const res = await fetch(location.href, {
        headers: { "X-Requested-With": "live" },
        cache: "no-store",
      });
      if (res.ok) {
        const fresh = new DOMParser()
          .parseFromString(await res.text(), "text/html")
          .querySelector("main");
        // Only touch the DOM on a real change, so scroll position and any
        // open <details> survive an unchanged poll.
        if (fresh && fresh.innerHTML !== main.innerHTML) {
          main.innerHTML = fresh.innerHTML;
        }
      }
    } catch {
      // A failed poll is not worth reporting; the next one will do.
    }
    schedule(onStatus ? 2000 : 3000);
  }

  let timer;
  function schedule(delay) {
    clearTimeout(timer);
    timer = setTimeout(tick, delay);
  }

  schedule(2000);
});
