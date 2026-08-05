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
  // A save that worked and a save that did not are not the same kind of news,
  // and used to be told the same way — both as a toast over the page.
  //
  // Succeeding is the ordinary case and wants the smallest possible remark: a
  // chip in the row of tools, next to the buttons that do things to this
  // document, gone again in under two seconds. Failing is the opposite. It
  // carries a reason, the edit is still unsaved, and it keeps the toast, which
  // stays until it is read.
  //
  // Nothing marks the save as in flight. It is one small request and the chip
  // is the answer; a "Saving…" that is replaced 80ms later reads as a flicker
  // rather than as progress. Model progress does report itself, in the
  // Naturalization timeline, because that runs for seconds and is worth
  // watching.
  const savedChip = document.querySelector(".tools .saved");
  let saveToast;
  let savedTimer;

  function setStatus(text, kind) {
    if (kind === "bad") {
      // Whatever the chip last claimed is no longer true.
      showSaved(false);
      saveToast?.remove();
      saveToast = window.toast ? toast(text, { kind, sticky: true }) : null;
      return;
    }
    // A save that succeeded clears an earlier failure: the edit did land in the
    // end, and a stuck "Not saved" is then a lie the reader has to dismiss.
    saveToast?.remove();
    saveToast = null;
    showSaved(kind === "ok");
  }

  function showSaved(on) {
    if (!savedChip) return;
    clearTimeout(savedTimer);
    savedChip.hidden = !on;
    if (on) savedTimer = setTimeout(() => (savedChip.hidden = true), 1800);
  }

  // --- timeline ----------------------------------------------------------
  // The server already knows how to write every step; rather than
  // reimplementing those labels here, the poll returns the rendered stages
  // and this applies them — which steps there are, in what order, as well as
  // what each one says. A page that watched the work happen therefore ends up
  // matching one loaded afterwards.
  const timeline = document.querySelector(".timeline");

  // The list of steps is itself part of what changes, not a fixed set of rows
  // with changing contents. Unlocking is the case that proves it: the page you
  // are returned to after typing a password has no unlock step, because at that
  // moment the document is merely processing and nothing has decrypted yet —
  // the step only exists once it has. Updating rows in place and skipping the
  // ones that were not already there left that step invisible until a reload,
  // which is the reload this whole mechanism is here to avoid.
  //
  // So the server's list wins outright: rows it names are created if missing,
  // put in its order, and rows it no longer names are dropped.
  function applyStages(stages) {
    if (!timeline || !Array.isArray(stages)) return;
    const seen = new Set();
    let prev = null;
    for (const st of stages) {
      seen.add(st.key);
      let row = timeline.querySelector(`li[data-stage="${st.key}"]`);
      if (!row) row = newRow(st.key);
      // Only when it is actually out of place: re-inserting a node restarts the
      // pulse on whichever step is working, and every poll would otherwise do
      // that to every row.
      const inPlace = prev ? prev.nextElementSibling === row : timeline.firstElementChild === row;
      if (!inPlace) {
        if (prev) prev.after(row);
        else timeline.prepend(row);
      }
      row.className = st.state;
      row.querySelector(".what").textContent = st.name;
      setLine(row, "cost", st.cost);
      setLine(row, "file", st.file);
      setLine(row, "detail", st.detail);
      prev = row;
    }
    for (const row of [...timeline.children]) {
      if (!seen.has(row.dataset.stage)) row.remove();
    }
  }

  // The same shape the template writes, so a row this builds and a row the
  // server rendered are the same row — which is what lets the page that watched
  // the work happen match the one loaded afterwards.
  function newRow(key) {
    const row = document.createElement("li");
    row.dataset.stage = key;
    const dot = document.createElement("span");
    dot.className = "dot";
    dot.setAttribute("aria-hidden", "true");
    const step = document.createElement("span");
    step.className = "step";
    const what = document.createElement("span");
    what.className = "what";
    step.append(what);
    row.append(dot, step);
    return row;
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
      // Signed out. Reload into the login flow. Anything typed since the last
      // autosave goes with it — but staying would not keep it either, since
      // every further save fails the same way; a dead page just loses it
      // slower.
      if (res.status === 401) {
        location.reload();
        throw new Error("signed out");
      }
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

  // Whether an edit is in the air: typed but not yet saved, or being saved
  // right now. Reloading over either would throw away what someone typed, so
  // the refresh at the end of processing asks first.
  let unsavedEdits = () => false;

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
      // Backspace deliberately does nothing here. The usual tag-input trick of
      // popping the last tag on backspace-in-an-empty-box destroys a tag from
      // a keystroke aimed at the text being typed, with nothing on screen
      // saying it happened — and the tag it takes is whichever was last in the
      // list, not one anybody pointed at. Removing a tag is the cross.
    }
  });

  render();

  unsavedEdits = () => dirty.size > 0 || saving;

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
    // The summary too, which used to be the one thing here that only a reload
    // could bring — and so the reason the whole page was fetched again at the
    // end. Now that the reload happens earlier, when the document itself
    // changes, this is what fills in afterwards.
    const summary = document.querySelector(".doc-summary");
    if (summary && typeof data.summary === "string" && data.summary) {
      summary.textContent = data.summary;
      summary.classList.remove("none");
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
      if (res.status === 401) {
        location.reload(); // signed out — reload into login
        return null;
      }
      if (!res.ok) return null;
      return await res.json();
    } catch {
      return null; // a transient failure should not end the wait
    }
  }

  // Watches until nothing is outstanding: the pipeline has finished and the
  // model is neither queued nor running.
  //
  // The page is fetched again the moment the pipeline finishes, not when
  // everything does. Those used to be the same moment; they are not any more,
  // because the model can be several minutes behind on a busy queue — and a
  // reader was made to wait all of it before seeing the PDF and the text that
  // had been ready the whole time. What arrives with the model afterwards is a
  // title, a summary, some tags and a date, and applyMeta writes all four in
  // place, so nothing is left for a second reload to do.
  async function watch(timeoutMs = 300000) {
    const deadline = Date.now() + timeoutMs;
    // Only a transition is worth reloading for. Landing on a page that is
    // already ready and merely waiting on the model must not reload it — and
    // must not do so every 1.2s forever.
    let wasProcessing = false;
    while (Date.now() < deadline) {
      await sleep(1200);
      const data = await poll();
      if (!data) continue;
      applyStages(data.stages);
      applyMeta(data);
      if (data.status === "processing") {
        wasProcessing = true;
        continue;
      }
      if (wasProcessing && data.status === "ready") {
        // The document is a different document now: a viewer pointed at an
        // archival PDF that did not exist when this page was drawn, a text
        // section that was empty, a new page count and file size.
        settle();
        return data;
      }
      if (!data.queued) return data;
    }
    return null;
  }

  // The pipeline has finished, and much of this page is now a description of a
  // document that no longer exists: a different page count and file size, a
  // text section that was empty and is now twelve thousand characters, and a
  // viewer pointed at an archival PDF that did not exist when the page was
  // drawn. None of that can be patched in field by field, so the page is
  // fetched again — once, here, and not again for the model.
  //
  // It used to wait for the model as well, which was the same moment back when
  // one document was enriched at a time behind whatever was ingesting. It is
  // not the same moment now, and waiting for it meant sitting in front of a
  // blank viewer while the finished PDF sat on disk.
  function settle() {
    // Except over an edit in progress. Autosave means a tag typed a moment ago
    // may still be in the air, and a reload would take it — so that page keeps
    // the in-place updates and at least gets its viewer back.
    const el = document.activeElement;
    const typing = el && /^(INPUT|TEXTAREA|SELECT)$/.test(el.tagName);
    if (typing || unsavedEdits()) {
      showPreview();
      return;
    }
    location.reload();
  }

  // The viewer alone, for when the page cannot be reloaded out from under
  // someone. It was pointed at an archival PDF that did not exist when the page
  // was drawn, so it is showing the error it got; now there is a file there.
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
    // Marked so the sweep below can tell what has already been wired without
    // naming it by where it posts.
    form.dataset.guarded = "1";
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
      if (res.status === 401) {
        location.reload(); // signed out — reload into login
        return;
      }
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
  //
  // Matched on the attribute that asks for the panel, not on where the form
  // posts. Matching the action's tail meant that giving trash a query string —
  // so it could return to the listing it came from — silently stopped it ending
  // in "/trash", the panel never bound, and the browser's own confirm box was
  // left to do the job. Which is a real difference in behaviour, appearing only
  // when a filter was on, from a change that had nothing to do with either.
  for (const form of document.querySelectorAll("form[data-confirms]:not([data-guarded])")) {
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
