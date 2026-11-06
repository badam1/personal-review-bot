// The Settings page: the bar that says what you've changed and saves it, the live previews (the certainty, the
// signature, the plan's zones), the Reset links, Copy path, and the editors that open over the page. Without
// JavaScript the form still saves: the bar stays put with its Save button, and the editors open from their links.
// Served from this origin; the CSP allows no inline script.
(() => {
  const form = document.querySelector("form[data-settings]");
  if (!form) return;
  const bar = form.querySelector("[data-savebar]");
  const count = bar.querySelector("[data-count]");
  const summary = bar.querySelector("[data-summary]");
  const discard = bar.querySelector("[data-discard]");
  const rows = [...form.querySelectorAll("[data-row]")];
  const unsaved = form.hasAttribute("data-unsaved"); // the server sent back a form it didn't save

  // What a row is set to, in words: "85%", "Low, Medium", "on".
  const labelOf = (el) => el.closest("label")?.textContent.trim() ?? el.value;
  const shown = (row) => {
    const parts = [];
    row.querySelectorAll("input, textarea").forEach((el) => {
      if (el.type === "hidden" || el.disabled || !el.name || el.name === "confirm_high") return;
      if (el.type === "radio") { if (el.checked) parts.push(labelOf(el)); }
      else if (el.type === "checkbox" && el.closest(".togs")) { if (el.checked) parts.push(labelOf(el)); }
      else if (el.type === "checkbox") parts.push(el.checked ? "on" : "off");
      else parts.push(el.value.trim() === "" ? "empty" : el.value.trim() + (row.dataset.unit ?? ""));
    });
    return parts.join(", ") || "none";
  };
  const before = new Map(rows.map((r) => [r, shown(r)]));

  const previews = () => {
    const out = form.querySelector('[data-out="THRESHOLD"]');
    const threshold = form.elements.THRESHOLD;
    if (out && threshold) out.textContent = threshold.value + "%";
    const sig = form.querySelector("[data-sig]");
    const watermark = form.elements.WATERMARK;
    if (sig && watermark) sig.textContent = watermark.value.trim() + " · 92%";
    const zones = form.querySelector("[data-zones]");
    const save = parseInt(form.elements.SAVE_AT?.value, 10);
    const cheapest = parseInt(form.elements.MIN_AT?.value, 10);
    if (zones && save >= 0 && save <= 100 && cheapest >= save && cheapest <= 100) {
      const widths = [save, cheapest - save, 100 - cheapest];
      zones.querySelectorAll(".zbar > .z1, .zbar > .z2, .zbar > .z3").forEach((z, i) => { z.style.width = widths[i] + "%"; });
      zones.querySelector(".zlabels").style.gridTemplateColumns = widths.map((w) => w + "%").join(" ");
    }
  };

  let dirty = [];
  const update = () => {
    previews();
    dirty = rows.filter((r) => shown(r) !== before.get(r));
    document.querySelectorAll("[data-changes-for]").forEach((n) => {
      const k = dirty.filter((r) => r.closest("section")?.id === n.dataset.changesFor).length;
      n.textContent = k === 0 ? "" : k === 1 ? "1 change" : k + " changes";
    });
    if (unsaved) return; // the bar already says so, and stays
    bar.hidden = dirty.length === 0;
    if (dirty.length === 0) return;
    count.textContent = dirty.length === 1 ? "1 unsaved change" : dirty.length + " unsaved changes";
    const said = dirty.slice(0, 2).map((r) => r.dataset.label +
      (r.hasAttribute("data-long") ? " changed" : ": " + before.get(r) + " → " + shown(r)));
    if (dirty.length > 2) said.push("and " + (dirty.length - 2) + " more");
    summary.textContent = said.join("; ") + ". Applies from the next review.";
  };
  form.addEventListener("input", update);
  form.addEventListener("change", update);
  discard.hidden = false;
  discard.addEventListener("click", () => { form.reset(); setTimeout(update); });

  // Reset links put the default back in the form; saving makes it so.
  form.querySelectorAll("[data-reset]").forEach((b) => {
    b.hidden = false;
    b.addEventListener("click", () => {
      const value = b.dataset.value;
      form.querySelectorAll(`[name="${b.dataset.reset}"]`).forEach((el) => {
        if (el.type === "hidden") return;
        if (el.type === "radio") el.checked = el.value === value;
        else if (el.type === "checkbox") el.checked = value.split(",").includes(el.value);
        else el.value = value;
      });
      update();
    });
  });

  // Leaving with changes asks first; saving doesn't.
  let saving = false;
  form.addEventListener("submit", () => { saving = true; });
  window.addEventListener("beforeunload", (e) => {
    if (!saving && dirty.length > 0) e.preventDefault();
  });

  document.querySelectorAll("[data-copy]").forEach((b) => {
    if (!navigator.clipboard) return;
    b.hidden = false;
    b.addEventListener("click", async () => {
      const label = b.lastChild;
      try {
        await navigator.clipboard.writeText(b.dataset.copy);
        label.textContent = "Copied";
        setTimeout(() => { label.textContent = "Copy path"; }, 1500);
      } catch { /* the browser said no: the path is on the page to select */ }
    });
  });

  // Buttons that delete one of your files ask first.
  document.querySelectorAll("button[data-confirm]").forEach((b) => {
    b.addEventListener("click", (e) => {
      if (!confirm(b.dataset.confirm)) e.preventDefault();
    });
  });

  // The editors: Escape closes the open one, and opening one puts you in its first field.
  const openDrawer = () => document.querySelector(".drawer:target, .drawer.open");
  const focusDrawer = () => openDrawer()?.querySelector("input:not([type=hidden]), textarea")?.focus();
  window.addEventListener("hashchange", focusDrawer);
  document.addEventListener("keydown", (e) => {
    if (e.key !== "Escape") return;
    const close = openDrawer()?.querySelector("a.scrim");
    if (close) location.href = close.href;
  });
  focusDrawer();

  // The section list marks the section you're reading: the last one whose top has gone under the bar, or the last
  // one of all once the page can't scroll further.
  const links = [...document.querySelectorAll(".set-nav a")];
  const sections = links.map((a) => document.getElementById(a.hash.slice(1))).filter(Boolean);
  let queued = false;
  const mark = () => {
    queued = false;
    let current = sections[0];
    // a link to a section leaves its top 140px down (scroll-padding-top), so that counts as reached
    sections.forEach((s) => { if (s.getBoundingClientRect().top <= 160) current = s; });
    if (window.innerHeight + window.scrollY >= document.documentElement.scrollHeight - 2) current = sections.at(-1);
    links.forEach((a) => a.setAttribute("aria-current", String(a.hash === "#" + current?.id)));
  };
  window.addEventListener("scroll", () => {
    if (!queued) {
      queued = true;
      requestAnimationFrame(mark);
    }
  }, { passive: true });
  mark();

  update();
})();
