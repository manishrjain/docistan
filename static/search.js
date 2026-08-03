// Search as you type. Typesense already matches prefixes and tolerates typos
// on every query we send, so a half-typed word is a perfectly good search
// already; all that stood between the reader and it was pressing Enter and
// waiting for a page.
//
// The server renders the regions that move with the search text and this swaps
// them in. Nothing here builds a row: the snippets are marked up by the server,
// which escapes the document text before it turns the markers into <mark>, and
// a second copy of that in JavaScript is the one place this feature could
// become an XSS hole.
//
// Enter still submits the form and navigates for real, so the box works with
// scripting off.

document.addEventListener("DOMContentLoaded", () => {
  const form = document.querySelector("form.searchbox");
  const input = form?.querySelector('input[name="q"]');
  // What /results answers with, by the id each region carries in the page as
  // well. Contents are replaced, never the elements: #results is the picker
  // <form> the page renders around the rows and Download submits, and the two
  // tag regions sit either side of #tags-open, the checkbox that holds the
  // popover open — replacing any of those nodes is the whole thing this is
  // arranged to avoid. The date controls and the view counts are not here:
  // neither moves with what is typed.
  const regions = ["results", "tag-pills", "tag-browse"];
  const region = document.getElementById("results");
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
      // Parsed rather than assigned: the response is several regions, and each
      // one has to reach the element it belongs to. A document built here is
      // inert — its scripts do not run and nothing in it loads — so this costs
      // one parse and no requests.
      const next = new DOMParser().parseFromString(html, "text/html");
      for (const id of regions) {
        const src = next.getElementById(id);
        const dst = document.getElementById(id);
        if (src && dst) dst.innerHTML = src.innerHTML;
      }
      // The checkboxes on screen are new nodes, so the running count and the
      // select-all have to be worked out again from them.
      document.dispatchEvent(new CustomEvent("picker:refresh"));
      // And the tag links filters.js was holding are gone, so what the reader
      // typed into the tag search has to be applied to the ones that replaced
      // them.
      document.dispatchEvent(new CustomEvent("tags:refresh"));
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
  //
  // "@47" is the exception: a document number typed in full is a request for
  // that document, so Enter opens it rather than a list of one. Only on Enter —
  // during typing, "@4" on the way to "@47" must not teleport anybody to DOC-4.
  //
  // The check is against the rows already on screen rather than a lookup,
  // because the results for what is in the box are what is being displayed: if
  // exactly one row is there and it is that number, it is the document meant. A
  // number that matches nothing, or a prefix matching several, submits normally
  // and shows the list. With scripting off, so does every search.
  form.addEventListener("submit", (e) => {
    clearTimeout(timer);
    inflight?.abort();

    const digits = /^\s*@(\d+)\s*$/.exec(input.value);
    if (!digits) return;
    const rows = region.querySelectorAll("[data-doc-id]");
    if (rows.length !== 1 || rows[0].dataset.docId !== digits[1]) return;
    e.preventDefault();
    location.assign("/doc/" + digits[1]);
  });
});
