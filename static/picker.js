// Selecting documents to act on. The checkboxes, the buttons, the escalation
// banner and the two tag popovers are a plain form and work without any of
// this; what follows adds the running count, the select-all, the shift-click
// range, and the two courtesies a <details> popover does not come with.
//
// Nothing is bound to a checkbox. The rows are not permanent — search.js swaps
// the results region in as the reader types, and live.js redraws <main> while
// an upload is still being processed — so a listener attached to the boxes
// found at load is attached to nodes that are thrown away moments later, and
// selection quietly stops working on everything that arrives afterwards. The
// listeners sit on the document instead, which is above both swaps, and a row
// that appears later is already wired. picker:refresh, fired by whoever did
// the swapping, only asks for the count to be worked out again.

document.addEventListener("DOMContentLoaded", () => {
  const picker = (el) => el?.closest?.("form.picker");
  const boxesIn = (form) => [...form.querySelectorAll(".pick:not(.pick-all)")];
  const hit = (b) => b.closest(".pick-hit") || b;
  const clearScope = (form) => {
    const scope = form?.querySelector(".pick-scope");
    if (scope) scope.checked = false;
  };

  function sync() {
    const form = document.querySelector("form.picker");
    if (!form) return;
    const all = form.querySelector(".pick-all");
    const boxes = boxesIn(form);
    if (!boxes.length) {
      // Nothing to act on, so the controls for acting on it are noise. A later
      // swap that has rows renders them again.
      form.querySelector(".picker-actions")?.remove();
      if (all) hit(all).remove();
      return;
    }

    const n = boxes.filter((b) => b.checked).length;
    const label = form.querySelector(".picked-count");
    if (label) {
      // Once the selection has been escalated past this page, the number of
      // boxes on screen is no longer what is selected: the form will post
      // scope=filtered and the server will act on every match. The total comes
      // from the server, on the element, because this file has no way to know
      // how many documents a query found. Grouped the way the page groups its
      // other numbers rather than printed raw.
      const total = Number(label.dataset.total || 0);
      const escalated = form.querySelector(".pick-scope")?.checked && total > 0;
      label.textContent = `${escalated ? total.toLocaleString() : n} selected`;
    }
    // Whether the actions are visible is the stylesheet's business, decided
    // from the checkboxes directly — this used to write a count here for a
    // selector to read, which meant the buttons could not be hidden until
    // this file had loaded and run.
    if (all) {
      all.checked = n === boxes.length;
      all.indeterminate = n > 0 && n < boxes.length;
    }
  }

  document.addEventListener("change", (e) => {
    // The escalation is not one of the selection's boxes — it is a claim about
    // all of them — but the count above the rows has to answer to it.
    if (e.target.closest?.(".pick-scope")) {
      sync();
      return;
    }

    const box = e.target.closest?.(".pick");
    const form = picker(box);
    if (!form) return;
    if (box.classList.contains("pick-all")) {
      for (const b of boxesIn(form)) b.checked = box.checked;
    }
    // "And every other page as well" was answered about a page that was
    // entirely ticked. The moment that stops being true the answer is stale,
    // and leaving it set would trash four thousand documents on behalf of
    // someone who had just un-ticked one.
    clearScope(form);
    sync();
  });

  // Shift-click selects the span between two boxes, the way a file manager
  // does — the difference between ticking four and ticking forty.
  let anchor = null;

  document.addEventListener("click", (e) => {
    const clear = e.target.closest?.(".picker-clear");
    if (clear && picker(clear)) {
      for (const b of boxesIn(picker(clear))) b.checked = false;
      // Including the escalation: Clear means nothing is selected, and an
      // escalation left ticked behind an empty page is a form that would still
      // post scope=filtered.
      clearScope(picker(clear));
      anchor = null;
      sync();
      return;
    }

    const box = e.target.closest?.(".pick:not(.pick-all)");
    const form = picker(box);
    if (!form) return;
    if (e.shiftKey && anchor && anchor !== box) {
      const boxes = boxesIn(form);
      const i = boxes.indexOf(anchor);
      const j = boxes.indexOf(box);
      // An anchor from before a swap is not in this list at all, and a range
      // to nowhere would tick the whole page.
      if (i >= 0 && j >= 0) {
        const [lo, hi] = [i, j].sort((x, y) => x - y);
        for (let k = lo; k <= hi; k++) boxes[k].checked = box.checked;
        sync();
      }
    }
    anchor = box;
  });

  // The two tag popovers. <details> opens, closes and takes focus on its own,
  // which is why it is the markup — what it does not do is close when the
  // reader's attention goes elsewhere, and something laid over the rows that
  // only closes when you find its summary again is a panel, not a popover.
  // Both handlers are on the document for the reason everything here is: the
  // results region these live in is replaced as the reader types.
  const closeTags = (keep) => {
    for (const d of document.querySelectorAll(".bulk-tag[open]")) {
      if (d !== keep) d.removeAttribute("open");
    }
  };

  document.addEventListener("click", (e) => {
    // The one being clicked is spared, which is also what closes the other:
    // the summary's own toggle happens after this, so opening either popover
    // shuts the one already open rather than leaving two over the rows.
    closeTags(e.target.closest?.(".bulk-tag"));
  });

  document.addEventListener("keydown", (e) => {
    if (e.key === "Escape") {
      closeTags(null);
      return;
    }
    // Enter in a tag box means that box's own button. A form submits through
    // its first submit button, which is the Add popover's — so Enter in the
    // Remove box would otherwise post an empty add and be answered with an
    // error. Unscripted it still does that, which is a wasted click rather
    // than a wrong document: an empty tag is refused, not applied.
    const pop = e.key === "Enter" && e.target.closest?.(".bulk-pop");
    if (pop) {
      e.preventDefault();
      pop.querySelector("button[type=submit]")?.click();
    }
  });

  // Rows arrived from the server: the count, the select-all and whether the
  // controls belong on screen all follow from boxes nobody has clicked yet.
  document.addEventListener("picker:refresh", sync);

  sync();
});
