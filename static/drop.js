// Drag a file anywhere in the app and it uploads. This is the one thing that
// genuinely needs the client: the browser owns the drag events and the file
// handles. Without it the upload form still works as a plain file picker.

const overlay = document.createElement("div");
overlay.className = "drop-overlay";
overlay.innerHTML =
  '<div>' +
  '<svg width="40" height="40" viewBox="0 0 24 24" fill="none" stroke="var(--color-accent)" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">' +
  '<path d="M12 16V4"/><path d="M7 9l5-5 5 5"/><path d="M4 16v3a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2v-3"/></svg>' +
  '<div class="big">Drop files anywhere</div>' +
  '<div class="micro">Welcome to Docistan — arrive, get processed, belong</div>' +
  "</div>";

document.addEventListener("DOMContentLoaded", () => {
  document.body.append(overlay);
});

// Only react to an actual file drag, not to text or link dragging.
const draggingFiles = (e) =>
  e.dataTransfer && Array.from(e.dataTransfer.types || []).includes("Files");

// dragenter/dragleave fire for every child element crossed, so nesting is
// counted rather than toggled — otherwise the overlay flickers.
let depth = 0;

window.addEventListener("dragenter", (e) => {
  if (!draggingFiles(e)) return;
  e.preventDefault();
  depth++;
  overlay.classList.add("on");
});

window.addEventListener("dragover", (e) => {
  if (draggingFiles(e)) e.preventDefault();
});

window.addEventListener("dragleave", (e) => {
  if (!draggingFiles(e)) return;
  depth = Math.max(0, depth - 1);
  if (depth === 0) overlay.classList.remove("on");
});

window.addEventListener("drop", async (e) => {
  if (!draggingFiles(e)) return;
  e.preventDefault();
  depth = 0;
  overlay.classList.remove("on");

  const files = [...(e.dataTransfer.files || [])];
  if (!files.length) return;

  const body = new FormData();
  for (const f of files) body.append("files", f);

  const pending = toast(
    `Uploading ${files.length} file${files.length === 1 ? "" : "s"}…`,
    { sticky: true },
  );

  try {
    const res = await fetch("/upload", {
      method: "POST",
      body,
      headers: { Accept: "application/json" },
    });
    if (!res.ok) throw new Error(`server said ${res.status}`);
    const data = await res.json();
    pending.remove();

    let queued = 0;
    for (const f of data.flash || []) {
      toast(f.text, {
        kind: f.bad ? "bad" : undefined,
        href: f.doc_id ? `/doc/${f.doc_id}` : undefined,
        linkText: f.doc_id ? `see #${f.doc_id}` : undefined,
      });
      if (!f.bad && !f.doc_id) queued++;
    }
    // Give the watcher a moment to pick the files up, then refresh so the new
    // documents show as processing.
    if (queued) setTimeout(() => location.reload(), 1500);
  } catch (err) {
    pending.remove();
    toast(`Upload failed: ${err.message}`, { kind: "bad" });
  }
});
