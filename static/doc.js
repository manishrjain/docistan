// Document metadata editing: tag pills with autocomplete, and autosave for
// every field. Progressive enhancement throughout — the markup is a plain
// form with a Save button, and this script upgrades it. With the script
// absent the page still works, just with commas typed by hand and an explicit
// Save.

document.addEventListener("DOMContentLoaded", () => {
  const form = document.querySelector(".doc-meta form");
  if (!form) return;

  const editor = form.querySelector(".tag-editor");
  const field = document.getElementById("tags-field");
  const heading = document.querySelector(".doc-meta h1");

  // --- save status -------------------------------------------------------
  const status = document.createElement("span");
  status.className = "save-status";
  form.prepend(status);

  let statusTimer;
  function setStatus(text, kind) {
    clearTimeout(statusTimer);
    status.textContent = text;
    status.className = "save-status" + (kind ? " " + kind : "");
    if (kind === "ok") {
      statusTimer = setTimeout(() => (status.textContent = ""), 2000);
    }
  }

  // --- autosave ----------------------------------------------------------
  // One save at a time. A change arriving mid-flight sets a flag rather than
  // firing a second request, so rapid edits collapse into one follow-up and
  // responses can never land out of order.
  let saving = false;
  let again = false;

  async function save() {
    if (saving) {
      again = true;
      return;
    }
    saving = true;
    setStatus("Saving…");

    try {
      const res = await fetch(form.action, {
        method: "POST",
        body: new FormData(form),
        headers: { Accept: "application/json" },
      });
      if (!res.ok) throw new Error(await res.text() || `HTTP ${res.status}`);
      const data = await res.json();
      if (heading && data.title) heading.textContent = data.title;
      setStatus("Saved", "ok");
    } catch (err) {
      // Never silently drop an edit: the field keeps the user's value and the
      // message stays until the next successful save.
      setStatus(`Not saved — ${String(err.message || err).slice(0, 120)}`, "bad");
    } finally {
      saving = false;
      if (again) {
        again = false;
        save();
      }
    }
  }

  let debounce;
  function saveSoon(delay = 700) {
    clearTimeout(debounce);
    debounce = setTimeout(save, delay);
  }

  // Typing saves shortly after you stop; leaving the field saves at once.
  for (const el of form.querySelectorAll('input[type="text"], input[type="month"]')) {
    if (el === field) continue;
    el.addEventListener("input", () => saveSoon());
    el.addEventListener("change", () => {
      clearTimeout(debounce);
      save();
    });
    el.addEventListener("blur", () => {
      clearTimeout(debounce);
      save();
    });
  }

  // Enter should commit rather than reload the page.
  form.addEventListener("submit", (e) => {
    e.preventDefault();
    clearTimeout(debounce);
    save();
  });

  // The button is redundant once edits save themselves.
  form.querySelector('button[type="submit"]')?.remove();

  // --- tag pills ---------------------------------------------------------
  if (!editor || !field) return;

  const known = (editor.dataset.known || "")
    .split(",")
    .map((t) => t.trim().toLowerCase())
    .filter(Boolean);

  let tags = parse(field.value);
  field.type = "hidden";

  const pills = document.createElement("div");
  pills.className = "pills";

  const box = document.createElement("div");
  box.className = "tag-input-row";

  const input = document.createElement("input");
  input.type = "text";
  input.className = "tag-input";
  input.placeholder = "Add a tag…";
  input.autocomplete = "off";
  input.setAttribute("aria-label", "Add a tag");

  const menu = document.createElement("ul");
  menu.className = "tag-suggest";
  menu.hidden = true;

  box.append(input, menu);
  editor.append(pills, box);

  function parse(value) {
    const seen = new Set();
    return value
      .split(",")
      .map((t) => t.trim().toLowerCase())
      .filter((t) => t && !seen.has(t) && seen.add(t));
  }

  // Tag changes are discrete decisions, not typing, so they save immediately
  // rather than waiting out a debounce.
  function sync() {
    field.value = tags.join(", ");
    render();
    clearTimeout(debounce);
    save();
  }

  function render() {
    pills.textContent = "";
    for (const tag of tags) {
      const pill = document.createElement("span");
      pill.className = "pill";

      const label = document.createElement("span");
      label.textContent = tag;

      const remove = document.createElement("button");
      remove.type = "button";
      remove.className = "pill-x";
      remove.textContent = "×";
      remove.title = `Remove ${tag}`;
      remove.setAttribute("aria-label", `Remove ${tag}`);
      remove.addEventListener("click", () => {
        tags = tags.filter((t) => t !== tag);
        sync();
        input.focus();
      });

      pill.append(label, remove);
      pills.append(pill);
    }
  }

  function add(raw) {
    const tag = raw.trim().toLowerCase().replace(/^[,\s]+|[,\s]+$/g, "");
    if (!tag || tags.includes(tag)) return;
    tags.push(tag);
    sync();
  }

  let active = -1;

  function suggestions() {
    const q = input.value.trim().toLowerCase();
    if (!q) return [];
    return known
      .filter((t) => t.includes(q) && !tags.includes(t))
      .sort((a, b) => a.indexOf(q) - b.indexOf(q) || a.length - b.length)
      .slice(0, 8);
  }

  function showMenu() {
    const items = suggestions();
    menu.textContent = "";
    active = -1;
    if (!items.length) {
      menu.hidden = true;
      return;
    }
    items.forEach((tag, i) => {
      const li = document.createElement("li");
      li.textContent = tag;
      li.addEventListener("mousedown", (e) => {
        e.preventDefault();
        add(tag);
        input.value = "";
        showMenu();
      });
      li.addEventListener("mouseenter", () => {
        active = i;
        highlight();
      });
      menu.append(li);
    });
    menu.hidden = false;
  }

  function highlight() {
    [...menu.children].forEach((li, i) => li.classList.toggle("on", i === active));
  }

  input.addEventListener("input", showMenu);
  input.addEventListener("blur", () => {
    setTimeout(() => (menu.hidden = true), 120);
    // A tag left half-typed should count rather than vanish.
    if (input.value.trim()) {
      add(input.value);
      input.value = "";
    }
  });

  input.addEventListener("keydown", (e) => {
    const items = [...menu.children];
    switch (e.key) {
      case "Enter":
      case ",":
        e.preventDefault();
        if (active >= 0 && items[active]) {
          add(items[active].textContent);
        } else {
          add(input.value);
        }
        input.value = "";
        showMenu();
        break;
      case "ArrowDown":
        if (items.length) {
          e.preventDefault();
          active = (active + 1) % items.length;
          highlight();
        }
        break;
      case "ArrowUp":
        if (items.length) {
          e.preventDefault();
          active = (active - 1 + items.length) % items.length;
          highlight();
        }
        break;
      case "Escape":
        menu.hidden = true;
        break;
      case "Backspace":
        if (!input.value && tags.length) {
          tags.pop();
          sync();
        }
        break;
    }
  });

  render();
});
