// Index-page filter niceties. Everything here is an enhancement: the tag
// browser is a <details>, the date range is a plain GET form, and both work
// unaided. This makes them quicker, not possible.

document.addEventListener("DOMContentLoaded", () => {
  // --- tag browser -------------------------------------------------------
  // The panel opens by itself: a checkbox on wide screens, and unconditionally
  // inside the narrow filter sheet. This only adds the search box, which is
  // no use without script and so ships hidden.
  const tags = document.querySelector(".filter-tags");
  const toggle = document.getElementById("tags-open");
  if (tags) {
    const filter = tags.querySelector(".tag-filter");

    if (filter) {
      // Re-read rather than held from load. The vocabulary narrows with the
      // result set, so search.js swaps this list in as the reader types and the
      // links captured once are nodes that left the page moments later — a
      // stale list is a tag filter that silently stops applying to what is on
      // screen. The box itself is outside the swap, so its text survives and is
      // what gets re-applied.
      let links = [];

      const apply = () => {
        const q = filter.value.trim().toLowerCase();
        for (const a of links) {
          a.hidden = !!q && !(a.dataset.tag || "").toLowerCase().includes(q);
        }
      };

      const reread = () => {
        links = [...tags.querySelectorAll(".tag-browse-list a")];
        // No use without script, and none with nothing to search either.
        filter.hidden = links.length === 0;
        apply();
      };

      reread();
      filter.addEventListener("input", apply);
      // Fired by whoever swapped the list in.
      document.addEventListener("tags:refresh", reread);
      // Opening the popover starts from the whole list rather than from
      // whatever was typed last time.
      toggle?.addEventListener("change", () => {
        if (!toggle.checked) return;
        filter.value = "";
        apply();
        filter.focus();
      });
    }

    // A popover that only closes by clicking its own trigger is a nuisance.
    // Inside the sheet there is no popover, so there is nothing to close.
    if (toggle) {
      document.addEventListener("click", (e) => {
        if (toggle.checked && !tags.contains(e.target)) toggle.checked = false;
      });
      document.addEventListener("keydown", (e) => {
        if (e.key === "Escape" && toggle.checked) toggle.checked = false;
      });
    }
  }

  // --- custom date range -------------------------------------------------
  // Changing a select applies immediately, so the Apply button is redundant
  // and goes away — the same trade the document form makes with Save.
  const range = document.querySelector(".range-custom");
  if (range) {
    for (const sel of range.querySelectorAll("select")) {
      sel.addEventListener("change", () => range.submit());
    }
    range.querySelector('button[type="submit"]')?.remove();
  }
});
