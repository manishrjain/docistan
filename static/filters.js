// Index-page filter niceties. Everything here is an enhancement: the tag
// browser is a <details>, the date range is a plain GET form, and both work
// unaided. This makes them quicker, not possible.

document.addEventListener("DOMContentLoaded", () => {
  // --- tag browser -------------------------------------------------------
  const browse = document.querySelector(".tag-browse");
  if (browse) {
    const filter = browse.querySelector(".tag-filter");
    const links = [...browse.querySelectorAll(".tag-browse-list a")];

    // Only revealed once it can actually do something.
    if (filter) {
      filter.hidden = false;
      filter.addEventListener("input", () => {
        const q = filter.value.trim().toLowerCase();
        for (const a of links) {
          a.hidden = !!q && !(a.dataset.tag || "").toLowerCase().includes(q);
        }
      });
      browse.addEventListener("toggle", () => {
        if (browse.open) {
          filter.value = "";
          for (const a of links) a.hidden = false;
          filter.focus();
        }
      });
    }

    // A popover that only closes by clicking its own trigger is a nuisance.
    document.addEventListener("click", (e) => {
      if (browse.open && !browse.contains(e.target)) browse.open = false;
    });
    document.addEventListener("keydown", (e) => {
      if (e.key === "Escape" && browse.open) browse.open = false;
    });
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
