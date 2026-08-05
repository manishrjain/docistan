// Uploading. Two ways in, one overlay: drag files anywhere in the app, or press
// Upload and pick them. Dragging is the thing that genuinely needs the client —
// the browser owns the drag events and the file handles — and the picker rides
// along on the same overlay because the design puts them in the same place.
//
// Without script the Upload button is still a link to /upload, which is a plain
// form with a file input and a submit. Nothing here is required to upload.

const overlay = document.createElement("div");
overlay.className = "drop-overlay";
overlay.innerHTML =
  "<div>" +
  '<svg width="40" height="40" viewBox="0 0 24 24" fill="none" stroke="var(--color-accent)" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">' +
  '<path d="M12 16V4"/><path d="M7 9l5-5 5 5"/><path d="M4 16v3a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2v-3"/></svg>' +
  '<div class="big">Drop files anywhere</div>' +
  '<div class="micro">Welcome to Docovia — arrive, get processed, belong</div>' +
  // Only shown when the overlay was opened on purpose. A drag is already
  // holding the files, so offering to choose some would be answering a
  // question nobody asked.
  '<div class="drop-actions">' +
  '<button type="button" class="btn btn-primary drop-pick">Choose files</button>' +
  '<button type="button" class="btn btn-secondary drop-cancel">Cancel</button>' +
  "</div></div>";

// The picker itself never appears: it exists to be clicked by the button above
// and to hand back a FileList. accept is left to the server's own answer on
// /upload — an unsupported file is refused there with a message either way.
const picker = document.createElement("input");
picker.type = "file";
picker.multiple = true;
picker.className = "hidden";

document.addEventListener("DOMContentLoaded", () => {
  document.body.append(overlay, picker);

  // The Upload button is a real link, so this only takes it over once there is
  // script to take it over with.
  for (const a of document.querySelectorAll('a[href="/upload"]')) {
    a.addEventListener("click", (e) => {
      e.preventDefault();
      open(true);
    });
  }
});

function open(picking) {
  overlay.classList.toggle("picking", !!picking);
  overlay.classList.add("on");
}

function close() {
  overlay.classList.remove("on", "picking");
  depth = 0;
}

overlay.addEventListener("click", (e) => {
  if (e.target.closest(".drop-pick")) picker.click();
  else if (e.target.closest(".drop-cancel")) close();
});

// Escape closes it, the way it closes anything else laid over the page. Only
// when it was opened deliberately: during a drag the overlay is a response to
// the pointer, and the drag ending takes it away by itself.
document.addEventListener("keydown", (e) => {
  if (e.key === "Escape" && overlay.classList.contains("picking")) close();
});

picker.addEventListener("change", () => {
  const files = [...(picker.files || [])];
  // Cleared so choosing the same file twice in a row still fires a change.
  picker.value = "";
  close();
  upload(files);
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
  open(false);
});

window.addEventListener("dragover", (e) => {
  if (draggingFiles(e)) e.preventDefault();
});

window.addEventListener("dragleave", (e) => {
  if (!draggingFiles(e)) return;
  depth = Math.max(0, depth - 1);
  if (depth === 0) close();
});

window.addEventListener("drop", (e) => {
  if (!draggingFiles(e)) return;
  e.preventDefault();
  close();
  upload([...(e.dataTransfer.files || [])]);
});

// One path for both ways in, so a dropped file and a chosen one are the same
// upload with the same reporting.
async function upload(files) {
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
    if (res.status === 401) {
      location.reload(); // signed out — reload into login, then drop the files again
      return;
    }
    if (!res.ok) throw new Error(`server said ${res.status}`);
    const data = await res.json();
    pending.remove();

    let queued = 0;
    for (const f of data.flash || []) {
      toast(f.text, {
        kind: f.bad ? "bad" : undefined,
        href: f.doc_id ? `/doc/${f.doc_id}` : undefined,
        linkText: f.doc_id ? `see DOC-${f.doc_id}` : undefined,
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
}
