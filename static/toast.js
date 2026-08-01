// Shared transient notifications. Kept in one place so a dropped upload and a
// saved edit report themselves the same way, in the same corner.

(function () {
  let host;

  function container() {
    if (!host) {
      host = document.querySelector(".toasts");
      if (!host) {
        host = document.createElement("div");
        host.className = "toasts";
        document.body.append(host);
      }
    }
    return host;
  }

  // toast(text, {kind: "ok"|"bad"|undefined, href, linkText, sticky, ttl})
  window.toast = function toast(text, opts = {}) {
    const el = document.createElement("div");
    el.className = "toast" + (opts.kind ? " " + opts.kind : "");

    const label = document.createElement("span");
    label.textContent = text;
    el.append(label);

    if (opts.href) {
      el.append(document.createTextNode(" "));
      const a = document.createElement("a");
      a.href = opts.href;
      a.textContent = opts.linkText || "open";
      el.append(a);
    }

    container().append(el);
    if (!opts.sticky) {
      setTimeout(() => el.remove(), opts.ttl || 4000);
    }
    return el;
  };
})();
