// Search as you type. Typesense already matches prefixes and tolerates typos
// on every query we send, so a half-typed word is a perfectly good search
// already; all that stood between the reader and it was pressing Enter and
// waiting for a page.
//
// The server renders the results region and this swaps it in. Nothing here
// builds a row: the snippets are marked up by the server, which escapes the
// document text before it turns the markers into <mark>, and a second copy of
// that in JavaScript is the one place this feature could become an XSS hole.
//
// Enter still submits the form and navigates for real, so the box works with
// scripting off — and so the filter bar, which this never swaps, gets its
// facets recounted.

document.addEventListener("DOMContentLoaded", () => {
  const form = document.querySelector("form.searchbox");
  const input = form?.querySelector('input[name="q"]');
  // The results region is the picker form's contents; the form node itself is
  // rendered by the page around the swapped block, so it survives every swap
  // and stays the thing Download submits.
  const region = document.querySelector("form.picker");
  // The search box is in the shared topbar and so appears on pages with no
  // results to swap. There, Enter and a full navigation are all there is.
  if (!form || !input || !region) return;

  let timer;
  let inflight = null;

  async function run() {
    // The query string the form would have submitted, so what typing shows and
    // what Enter shows can never be two different result sets.
    const qs = new URLSearchParams(new FormData(form)).toString();

    // Shareable and bookmarkable at every keystroke — but replaceState, not
    // pushState: one history entry per letter typed makes Back useless.
    history.replaceState(null, "", qs ? "/?" + qs : "/");

    // The response for "ocea" must never land after the one for "oceanside"
    // and put the wider result set back on screen. Aborting whatever is still
    // in flight is what makes the last keystroke the one that wins.
    inflight?.abort();
    const ctl = new AbortController();
    inflight = ctl;
    region.setAttribute("aria-busy", "true");

    try {
      const res = await fetch("/results" + (qs ? "?" + qs : ""), {
        signal: ctl.signal,
        headers: { "X-Requested-With": "search" },
        cache: "no-store",
      });
      if (!res.ok || ctl.signal.aborted) return;
      const html = await res.text();
      if (ctl.signal.aborted) return;
      region.innerHTML = html;
      // The checkboxes on screen are new nodes, so the running count and the
      // select-all have to be worked out again from them.
      document.dispatchEvent(new CustomEvent("picker:refresh"));
    } catch {
      // An abort, or a request that failed. Either way the results already on
      // screen stay: being one keystroke behind beats an empty page.
    } finally {
      if (inflight === ctl) {
        inflight = null;
        region.removeAttribute("aria-busy");
      }
    }
  }

  input.addEventListener("input", () => {
    clearTimeout(timer);
    timer = setTimeout(run, 130);
  });

  // A submit is a real navigation, and the request this was about to make is
  // then a request for a page that is being replaced.
  form.addEventListener("submit", () => {
    clearTimeout(timer);
    inflight?.abort();
  });
});
