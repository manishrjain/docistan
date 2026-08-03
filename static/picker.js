// Selecting documents to download. The checkboxes and the Download button are
// a plain form and work without any of this; what follows adds the running
// count, the select-all, and the shift-click range.
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
    if (label) label.textContent = `${n} selected`;
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
    const box = e.target.closest?.(".pick");
    const form = picker(box);
    if (!form) return;
    if (box.classList.contains("pick-all")) {
      for (const b of boxesIn(form)) b.checked = box.checked;
    }
    sync();
  });

  // Shift-click selects the span between two boxes, the way a file manager
  // does — the difference between ticking four and ticking forty.
  let anchor = null;

  document.addEventListener("click", (e) => {
    const clear = e.target.closest?.(".picker-clear");
    if (clear && picker(clear)) {
      for (const b of boxesIn(picker(clear))) b.checked = false;
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

  // Rows arrived from the server: the count, the select-all and whether the
  // controls belong on screen all follow from boxes nobody has clicked yet.
  document.addEventListener("picker:refresh", sync);

  sync();
});
