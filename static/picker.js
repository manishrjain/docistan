// Selecting documents to download. The checkboxes and the Download button are
// a plain form and work without any of this; what follows adds the running
// count, the select-all, and hiding the actions until there is something to
// act on.

document.addEventListener("DOMContentLoaded", () => {
  const form = document.querySelector("form.picker");
  if (!form) return;

  const all = form.querySelector(".pick-all");
  const boxes = [...form.querySelectorAll(".pick:not(.pick-all)")];
  const hit = (b) => b.closest(".pick-hit") || b;
  const actions = form.querySelector(".picker-actions");
  const label = form.querySelector(".picked-count");
  const clear = form.querySelector(".picker-clear");
  if (!boxes.length) {
    actions?.remove();
    if (all) hit(all).remove();
    return;
  }

  function sync() {
    const n = boxes.filter((b) => b.checked).length;
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

  all?.addEventListener("change", () => {
    for (const b of boxes) b.checked = all.checked;
    sync();
  });
  for (const b of boxes) b.addEventListener("change", sync);

  clear?.addEventListener("click", () => {
    for (const b of boxes) b.checked = false;
    sync();
  });

  // Shift-click selects the span between two boxes, the way a file manager
  // does — the difference between ticking four and ticking forty.
  let anchor = null;
  for (const b of boxes) {
    b.addEventListener("click", (e) => {
      if (e.shiftKey && anchor && anchor !== b) {
        const [i, j] = [boxes.indexOf(anchor), boxes.indexOf(b)].sort((x, y) => x - y);
        for (let k = i; k <= j; k++) boxes[k].checked = b.checked;
        sync();
      }
      anchor = b;
    });
  }

  sync();
});
