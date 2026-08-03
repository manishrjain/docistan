// Document metadata editing: tag pills with autocomplete, and autosave for
// every field. Progressive enhancement throughout — the markup is a plain
// form with a Save button, and this script upgrades it. With the script
// absent the page still works, just with commas typed by hand and an explicit
// Save.

document.addEventListener("DOMContentLoaded", () => {
  const form = document.querySelector(".doc-form");
  if (!form) return;

  const editor = form.querySelector(".tag-editor");
  const field = document.getElementById("tags-field");
  const heading = document.querySelector(".doc-title");

  // --- feedback ----------------------------------------------------------
  // Edits report themselves as toasts, which are hard to miss. Model progress
  // reports itself in the Naturalization timeline instead, next to the other
  // steps, because that is where a reader already looks to see what ran.
  let saveToast;
  function setStatus(text, kind) {
    saveToast?.remove();
    saveToast = window.toast
      ? toast(text, { kind, sticky: !kind })
      : null;
  }

  // --- timeline ----------------------------------------------------------
  // The server already knows how to write every step; rather than
  // reimplementing those labels here, the poll returns the rendered stages
  // and this applies them. A page that watched the work happen therefore
  // ends up byte-identical to one loaded afterwards.
  const timeline = document.querySelector(".timeline");

  function applyStages(stages) {
    if (!timeline || !Array.isArray(stages)) return;
    for (const st of stages) {
      const row = timeline.querySelector(`li[data-stage="${st.key}"]`);
      if (!row) continue;
      row.className = st.state;
      row.querySelector(".what").textContent = st.name;
      setLine(row, "cost", st.cost);
      setLine(row, "file", st.file);
      setLine(row, "detail", st.detail);
    }
  }

  // Order matters: cost, then file, then the timestamp. A line that appears
  // mid-poll has to land in the right place, not at the end.
  const lineOrder = ["cost", "file", "detail"];
  function setLine(row, cls, text) {
    let el = row.querySelector("." + cls);
    if (!text) {
      el?.remove();
      return;
    }
    if (!el) {
      el = document.createElement("span");
      el.className = cls;
      const after = lineOrder.slice(lineOrder.indexOf(cls) + 1);
      const before = after.map((c) => row.querySelector("." + c)).find(Boolean);
      if (before) before.before(el);
      else row.querySelector(".step").append(el);
    }
    el.textContent = text;
    if (cls !== "detail") el.title = text;
  }

  // Used before the first poll comes back, so a press registers immediately
  // rather than looking ignored for a second.
  function markWorking(key, state, detail) {
    const row = timeline?.querySelector(`li[data-stage="${key}"]`);
    if (!row) return;
    row.className = state;
    setLine(row, "detail", detail);
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
      if (heading && data.title && !heading.dataset.editing) {
        heading.textContent = data.title;
      }
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

  // --- inline title and date ---------------------------------------------
  // The title used to appear twice: once as the heading and again as a form
  // field. The heading is now the editor, and the plain fields stay in the
  // markup only so the page still works without JavaScript.
  function inlineEdit(display, input, format) {
    if (!display || !input) return;
    input.closest("label")?.classList.add("hidden");

    display.addEventListener("click", start);
    display.addEventListener("keydown", (e) => {
      if (e.key === "Enter" || e.key === " ") {
        e.preventDefault();
        start();
      }
    });

    function start() {
      if (display.dataset.editing) return;
      display.dataset.editing = "1";

      const box = document.createElement("input");
      box.type = input.type === "month" ? "month" : "text";
      box.className = "inline-edit";
      box.value = input.value;

      const finish = (commit) => {
        if (!display.dataset.editing) return;
        delete display.dataset.editing;
        if (commit && box.value !== input.value) {
          input.value = box.value;
          save(input.name);
        }
        display.textContent = format(input.value);
        // The date reads as a placeholder rather than a value when unset.
        display.classList.toggle("unset", !input.value);
        box.remove();
        display.hidden = false;
      };

      box.addEventListener("keydown", (e) => {
        if (e.key === "Enter") {
          e.preventDefault();
          finish(true);
        }
        if (e.key === "Escape") finish(false);
      });
      box.addEventListener("blur", () => finish(true));

      display.hidden = true;
      display.after(box);
      box.focus();
      box.select?.();
    }
  }

  const monthNames = ["Jan","Feb","Mar","Apr","May","Jun","Jul","Aug","Sep","Oct","Nov","Dec"];
  function monthLabel(v) {
    const m = /^(\d{4})-(\d{2})$/.exec(v || "");
    return m ? `${monthNames[+m[2] - 1]} ${m[1]}` : "Add a date";
  }

  const titleDisplay = document.querySelector('[data-edits="title"]');
  const dateDisplay = document.querySelector('[data-edits="created_date"]');
  inlineEdit(titleDisplay, form.elements["title"], (v) => v || "Untitled");
  inlineEdit(dateDisplay, form.elements["created_date"], monthLabel);

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

  // The search box stays out of the way until asked for: a row of tags is
  // something to read, and an always-open text field makes it look like a
  // form that is waiting on you.
  const addBtn = document.createElement("button");
  addBtn.type = "button";
  addBtn.className = "tag-add";
  addBtn.textContent = "+ Add tag";

  const box = document.createElement("div");
  box.className = "tag-input-row";
  box.hidden = true;

  const input = document.createElement("input");
  input.type = "text";
  input.className = "tag-input";
  input.placeholder = "Search tags…";
  input.autocomplete = "off";
  input.setAttribute("aria-label", "Add a tag");

  const menu = document.createElement("ul");
  menu.className = "tag-suggest";
  menu.hidden = true;

  box.append(input, menu);
  editor.append(pills, box);

  function openAdder() {
    box.hidden = false;
    input.focus();
  }
  function closeAdder() {
    box.hidden = true;
    menu.hidden = true;
    input.value = "";
  }
  addBtn.addEventListener("click", () => (box.hidden ? openAdder() : closeAdder()));

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
    // The adder trails the pills, so it reads as the next one in the row.
    pills.append(addBtn);
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
    // Deferred, so a click landing on a suggestion is not cut off by the
    // field closing under it.
    setTimeout(() => {
      if (document.activeElement !== input) closeAdder();
    }, 120);
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
        if (menu.hidden) {
          closeAdder();
        } else {
          menu.hidden = true;
        }
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
      const t = form.elements["title"];
      if (t) t.value = data.title;
      if (heading && !heading.dataset.editing) heading.textContent = data.title;
    }
    if (typeof data.created_date === "string") {
      const d = form.elements["created_date"];
      if (d) d.value = data.created_date;
      if (dateDisplay && !dateDisplay.dataset.editing) {
        dateDisplay.textContent = monthLabel(data.created_date);
        dateDisplay.classList.toggle("unset", !data.created_date);
      }
    }
  };
  }

  // --- reprocess and re-tag ----------------------------------------------
  // Both run in the background, so both stay on this page and report
  // themselves in the timeline. Neither redirects: the progress is here.

  const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

  async function poll() {
    try {
      const res = await fetch(`${location.pathname}/meta`, {
        headers: { Accept: "application/json" },
      });
      if (!res.ok) return null;
      return await res.json();
    } catch {
      return null; // a transient failure should not end the wait
    }
  }

  // Watches until nothing is outstanding: the pipeline has finished and the
  // model is neither queued nor running.
  async function watch(timeoutMs = 300000) {
    const deadline = Date.now() + timeoutMs;
    while (Date.now() < deadline) {
      await sleep(1200);
      const data = await poll();
      if (!data) continue;
      applyStages(data.stages);
      applyMeta(data);
      if (data.status !== "processing" && !data.queued) {
        if (data.status === "ready") showPreview();
        return data;
      }
    }
    return null;
  }

  // The viewer was pointed at an archival PDF that did not exist when the page
  // was drawn — the document was still being processed, or was locked and had
  // never been readable at all — so it is showing the error it got. Now that
  // there is a file there, load it.
  //
  // Reloading the frame rather than the page: the timeline, the tags and the
  // title have all just been updated in place, and throwing that away to fetch
  // the same information again would undo the point of watching at all.
  function showPreview() {
    const frame = document.querySelector(".doc-preview iframe");
    if (!frame) return;
    try {
      frame.contentWindow.location.reload();
    } catch {
      // Same origin, so the reload above is the normal path; this is for a
      // viewer that has navigated somewhere the parent may no longer touch.
      frame.src = frame.src;
    }
  }

  // Confirmation, when the markup asks for one, then the action. Without
  // JavaScript the form keeps its inline confirm() and posts normally; this
  // replaces that with the panel, which has room to say what happens.
  function guarded(form, run) {
    if (!form) return;
    const key = form.dataset.confirms;
    const panel = key
      ? document.querySelector(`.confirm[data-confirm="${key}"]`)
      : null;

    form.removeAttribute("onsubmit");
    form.addEventListener("submit", (e) => {
      e.preventDefault();
      if (!panel) {
        run();
        return;
      }
      for (const other of document.querySelectorAll(".confirm")) other.hidden = true;
      panel.hidden = false;
      panel.querySelector("[data-go]")?.focus();
    });
    if (!panel) return;
    panel.querySelector("[data-cancel]")?.addEventListener("click", () => {
      panel.hidden = true;
    });
    panel.querySelector("[data-go]")?.addEventListener("click", () => {
      panel.hidden = true;
      run();
    });
  }

  // Runs a form's action as a background request and watches it land, with
  // the button held disabled so it cannot be pressed twice.
  async function background(form, onAccepted) {
    const button = form.querySelector("button");
    if (button) button.disabled = true;
    try {
      const res = await fetch(form.action, {
        method: "POST",
        headers: { Accept: "application/json" },
      });
      if (!res.ok) throw new Error((await res.text()) || `HTTP ${res.status}`);
      const data = await res.json();
      if (onAccepted && onAccepted(data) === false) return;
      // No success message: the timeline says it finished, and saying so
      // twice over the top of it is noise. A failure still gets a toast,
      // because nothing changed on the page to show it.
      await watch();
    } catch (err) {
      if (window.toast) {
        toast(String(err.message || err).slice(0, 160), { kind: "bad" });
      }
    } finally {
      if (button) button.disabled = false;
    }
  }

  guarded(document.querySelector('form[action$="/retry"]'), () =>
    background(document.querySelector('form[action$="/retry"]'), () => {
      // The design's shape: the read is running, the model is behind it.
      markWorking("text", "working", "Working…");
      markWorking("tagging", "pending", "Queued");
    }),
  );

  guarded(document.querySelector('form[action$="/enrich"]'), () =>
    background(document.querySelector('form[action$="/enrich"]'), (data) => {
      if (data.status === "blocked") {
        markWorking("tagging", "failed", "Stopped: " + (data.reason || "the model is unavailable"));
        return false;
      }
      markWorking(
        "tagging",
        data.status === "waiting" ? "pending" : "working",
        data.reason ? "Waiting — " + data.reason : "Working…",
      );
    }),
  );

  // Trashing and deleting both navigate away, so they are the actions with
  // nothing to watch: the panel confirms, then the form posts as it would
  // have. Restore is not here — it needs no confirmation, so it is left as the
  // plain form it already is.
  for (const sel of ['form[action$="/trash"]', 'form[action$="/delete"]']) {
    const form = document.querySelector(sel);
    guarded(form, () => form.submit());
  }

  // A document still mid-flight when the page opened — ingested moments ago,
  // or reprocessing in another tab — is watched without anyone pressing
  // anything.
  if (timeline?.querySelector("li.pending, li.working")) watch();

  // --- unlock -------------------------------------------------------------
  // The form posts and navigates like any other action here; qpdf takes a
  // moment on a large document, so the button says what is happening rather
  // than sitting there looking unpressed. Nothing about whether the unlock
  // works depends on this running.
  const unlock = document.querySelector("form.unlock");
  unlock?.addEventListener("submit", () => {
    const button = unlock.querySelector('button[type="submit"]');
    if (!button) return;
    button.textContent = "Unlocking…";
    button.classList.replace("btn-primary", "btn-secondary");
    // Not disabled: a disabled submit button is not submitted, and this one
    // carries no value — but the form is already on its way, and re-pressing
    // it would only send the same password twice.
    button.style.pointerEvents = "none";
  });

  // --- document number ---------------------------------------------------
  // The number is what gets written on the paper original, so copying it
  // should not mean selecting six pixels of monospace by hand.
  const codeChip = document.querySelector(".doc-head .code[data-copy]");
  if (codeChip && navigator.clipboard) {
    const original = codeChip.textContent;
    codeChip.addEventListener("click", async () => {
      try {
        await navigator.clipboard.writeText(codeChip.dataset.copy);
        codeChip.textContent = "Copied ✓";
        setTimeout(() => (codeChip.textContent = original), 1400);
      } catch {
        // Clipboard access can be refused; the number is still on screen.
      }
    });
  }

});
