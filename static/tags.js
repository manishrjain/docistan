// Turns the plain comma-separated tags field into pills with removal and
// autocomplete. Progressive enhancement: the field is a normal text input in
// the markup and stays the thing the form posts, so with this script absent
// the page still works, just with commas typed by hand.

document.addEventListener("DOMContentLoaded", () => {
  const editor = document.querySelector(".tag-editor");
  const field = document.getElementById("tags-field");
  if (!editor || !field) return;

  const known = (editor.dataset.known || "")
    .split(",")
    .map((t) => t.trim().toLowerCase())
    .filter(Boolean);

  let tags = parse(field.value);

  // The real field keeps its name and value; it just stops being visible.
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

  function sync() {
    field.value = tags.join(", ");
    render();
  }

  function render() {
    pills.textContent = "";
    for (const tag of tags) {
      const pill = document.createElement("span");
      pill.className = "pill";

      const label = document.createElement("span");
      label.textContent = tag;

      const remove = document.createElement("button");
      remove.type = "button"; // never submit the form
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

  // Suggestions are existing tags only. Anything unmatched is simply created
  // on Enter, so there is no separate "create" affordance to hunt for.
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
        e.preventDefault(); // keep focus so blur doesn't close first
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
  input.addEventListener("blur", () => setTimeout(() => (menu.hidden = true), 120));

  input.addEventListener("keydown", (e) => {
    const items = [...menu.children];
    switch (e.key) {
      case "Enter":
      case ",":
        // Enter must not submit the form; adding a tag is the expected result.
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
        // Backspace on an empty box removes the last pill, which is the
        // convention every other tag input follows.
        if (!input.value && tags.length) {
          tags.pop();
          sync();
        }
        break;
    }
  });

  // A tag half-typed when Save is pressed should count, not vanish.
  field.form?.addEventListener("submit", () => {
    if (input.value.trim()) add(input.value);
  });

  render();
});
