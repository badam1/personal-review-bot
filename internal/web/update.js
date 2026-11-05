// The update notice while an update runs: its steps live from /update.json, "restarting" while the daemon is down,
// and "updated" once it is back on the new version, then a fresh page. Served from this origin; the CSP allows no
// inline script.
(() => {
  const box = document.getElementById("update");
  if (!box || !box.hasAttribute("data-busy")) return;
  const from = box.dataset.version;
  const state = box.querySelector(".update-state");
  const spin = box.querySelector(".spin");
  const glyphs = ["·", "✢", "✳", "✶", "✻", "✽", "✻", "✶", "✳", "✢"];
  let g = 0;
  const spinner = setInterval(() => { spin.textContent = glyphs[g++ % glyphs.length]; }, 120);
  const giveUp = Date.now() + 15 * 60 * 1000;

  const poll = async () => {
    try {
      const resp = await fetch("/update.json", { cache: "no-store" });
      if (resp.status === 404) { // back, as a version from before this page could follow updates
        location.replace("/");
        return;
      }
      if (!resp.ok) throw new Error(resp.status);
      const s = await resp.json();
      if (s.version !== from) { // back, as the new version
        clearInterval(spinner);
        spin.textContent = "✓";
        state.textContent = "updated to " + s.version + ", reloading…";
        setTimeout(() => location.replace("/"), 1500);
        return;
      }
      if (!s.busy) { // failed, or over: the page says how
        location.replace("/");
        return;
      }
      state.textContent = s.state;
    } catch {
      state.textContent = "restarting…"; // the daemon is between the old and the new binary
    }
    if (Date.now() < giveUp) setTimeout(poll, 1000);
  };
  setTimeout(poll, 1000);
})();
