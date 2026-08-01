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
  // Only the fields that actually changed are sent, and the server applies
  // only what it receives. Posting the whole form meant any request missing a
  // field blanked it, which is a data-loss bug waiting for a client change.
  //
  // One request at a time: further edits accumulate in `dirty` and go out in a
  // single follow-up, so responses cannot land out of order.
  let saving = false;
  const dirty = new Set();

  async function flush() {
    if (saving || dirty.size === 0) return;
    saving = true;

    const names = [...dirty];
    dirty.clear();

    const body = new URLSearchParams();
    for (const name of names) {
      const el = form.elements[name];
      if (el) body.set(name, el.value);
    }

    setStatus("Saving…");
    try {
      const res = await fetch(form.action, {
        method: "POST",
        body,
        headers: {
          Accept: "application/json",
          "Content-Type": "application/x-www-form-urlencoded",
        },
      });
      if (!res.ok) throw new Error((await res.text()) || `HTTP ${res.status}`);
      const data = await res.json();
      if (heading && data.title) heading.textContent = data.title;
      setStatus("Saved", "ok");
    } catch (err) {
      // Put the fields back so a later save retries them rather than dropping
      // the edit on the floor.
      names.forEach((n) => dirty.add(n));
      setStatus(`Not saved — ${String(err.message || err).slice(0, 120)}`, "bad");
    } finally {
      saving = false;
      if (dirty.size) flush();
    }
  }

  function save(...names) {
    names.forEach((n) => dirty.add(n));
    flush();
  }

  let debounce;
  function saveSoon(name, delay = 700) {
    dirty.add(name);
    clearTimeout(debounce);
    debounce = setTimeout(flush, delay);
  }

  // Typing saves shortly after you stop; leaving the field saves at once.
  for (const el of form.querySelectorAll('input[type="text"], input[type="month"]')) {
    if (el === field || !el.name) continue;
    el.addEventListener("input", () => saveSoon(el.name));
    el.addEventListener("change", () => {
      clearTimeout(debounce);
      save(el.name);
    });
    el.addEventListener("blur", () => {
      clearTimeout(debounce);
      save(el.name);
    });
  }

  // Enter should commit rather than reload the page.
  form.addEventListener("submit", (e) => {
    e.preventDefault();
    clearTimeout(debounce);
    save("title", "created_date", "tags");
  });

  // The button is redundant once edits save themselves.
  form.querySelector('button[type="submit"]')?.remove();

  // --- tag pills ---------------------------------------------------------
  // Exposed so background re-tagging can refresh the pills in place.
  let applyMeta = () => {};

  if (editor && field) {
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
    // A tag going on or off is a decision, not typing, so it saves at once —
    // and sends only the tags field.
    save("tags");
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

  applyMeta = (data) => {
    if (Array.isArray(data.tags)) {
      tags = data.tags.map((t) => String(t).toLowerCase());
      field.value = tags.join(", ");
      render();
    }
    if (typeof data.title === "string" && data.title) {
      if (heading) heading.textContent = data.title;
      const t = form.elements["title"];
      if (t) t.value = data.title;
    }
    if (typeof data.created_date === "string") {
      const d = form.elements["created_date"];
      if (d) d.value = data.created_date;
    }
  };
  }

  // --- re-tag with AI ----------------------------------------------------
  // The work happens in the background queue, so the button reports what is
  // happening and then polls until the result lands, rather than redirecting
  // to a page that looks unchanged.
  const retag = document.querySelector('form[action$="/enrich"]');
  if (retag) {
    const button = retag.querySelector("button");

    retag.addEventListener("submit", async (e) => {
      e.preventDefault();
      button.disabled = true;
      setStatus("Asking the model…");

      try {
        const res = await fetch(retag.action, {
          method: "POST",
          headers: { Accept: "application/json" },
        });
        if (!res.ok) {
          throw new Error((await res.text()) || `HTTP ${res.status}`);
        }
        const data = await res.json();
        if (data.status === "blocked") {
          setStatus(data.reason || "Tagging is stopped", "bad");
          button.disabled = false;
          return;
        }
        if (data.status === "waiting") {
          setStatus(`Queued — ${data.reason}`);
        }
        await waitForTags();
      } catch (err) {
        setStatus(`Could not re-tag — ${String(err.message || err).slice(0, 120)}`, "bad");
      } finally {
        button.disabled = false;
      }
    });

    async function waitForTags() {
      const deadline = Date.now() + 120000;
      let dots = 0;
      while (Date.now() < deadline) {
        await new Promise((r) => setTimeout(r, 1200));
        let data;
        try {
          const res = await fetch(`${location.pathname}/meta`, {
            headers: { Accept: "application/json" },
          });
          data = await res.json();
        } catch {
          continue; // a transient failure should not end the wait
        }

        if (data.enriched) {
          applyMeta(data);
          setStatus(`Tagged — ${data.title || "done"}`, "ok");
          return;
        }
        if (!data.queued) {
          setStatus("The model call did not succeed — see Processing below", "bad");
          return;
        }
        dots = (dots + 1) % 4;
        setStatus(
          data.reason
            ? `Waiting — ${data.reason}`
            : "Asking the model" + ".".repeat(dots),
        );
      }
      setStatus("Still queued — this page will show the tags once it runs");
    }
  }
});
