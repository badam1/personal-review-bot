// The version menu in the bar. It closes on a click elsewhere or on Escape. While an update runs, its steps follow
// /update.json live: "restarting" while the daemon is down, "updated" once it is back on the new version, then a fresh
// page. Served from this origin; the CSP allows no inline script.
(() => {
  const menu = document.getElementById("vmenu");
  if (!menu) return;
  const summary = menu.querySelector("summary");
  document.addEventListener("click", (e) => {
    if (menu.open && !menu.contains(e.target)) menu.open = false;
  });
  document.addEventListener("keydown", (e) => {
    if (e.key !== "Escape" || !menu.open) return;
    menu.open = false;
    summary.focus();
  });
  if (!menu.hasAttribute("data-busy")) return;

  const from = menu.dataset.version;
  const spin = summary.querySelector(".spin");
  const verb = summary.querySelector(".verb");
  const title = menu.querySelector(".panel .ttl");
  const bar = menu.querySelector(".panel .bar span");
  const items = [...menu.querySelectorAll(".usteps li")];
  const calm = window.matchMedia("(prefers-reduced-motion: reduce)").matches;
  const glyphs = ["·", "✢", "✳", "✶", "✻", "✽", "✻", "✶", "✳", "✢"];
  let g = 0;
  const spinner = calm ? 0 : setInterval(() => { spin.textContent = glyphs[g++ % glyphs.length]; }, 120);
  const giveUp = Date.now() + 15 * 60 * 1000;

  // The same steps and labels as updateSteps and stepLabel in view.go.
  const steps = [
    ["Download", "Downloaded", "downloading"],
    ["Check the checksum and build attestation", "Checksum and build attestation verified", "checking"],
    ["Install", "Installed", "installing"],
    ["Wait for running reviews", "Running reviews finished", ""],
    ["Restart", "Restarted", "restarting"],
  ];
  const label = (state) => {
    const i = state.indexOf(": ");
    if (i >= 0 && state.startsWith("v")) state = state.slice(i + 2);
    return state.charAt(0).toUpperCase() + state.slice(1);
  };
  const show = (state) => {
    let at = 0;
    if (state.includes("installed: restarting")) at = 3;
    else steps.forEach(([, , prefix], i) => { if (prefix && state.startsWith(prefix)) at = i; });
    items.forEach((li, i) => {
      li.className = i < at ? "done" : i === at ? "now" : "todo";
      li.querySelector(".label").textContent = i < at ? steps[i][1] : i === at ? label(state) : steps[i][0];
    });
    bar.style.width = Math.round((at * 100 + 50) / steps.length) + "%";
  };
  // A fresh page, without the "Update started." note in its address.
  const fresh = () => {
    const url = new URL(location.href);
    url.searchParams.delete("msg");
    location.replace(url.pathname + url.search);
  };

  const poll = async () => {
    try {
      const resp = await fetch("/update.json", { cache: "no-store" });
      if (resp.status === 404) { // back, as a version from before this page could follow updates
        fresh();
        return;
      }
      if (!resp.ok) throw new Error(resp.status);
      const s = await resp.json();
      if (s.version !== from) { // back, as the new version
        clearInterval(spinner);
        spin.textContent = "✓";
        verb.textContent = "Updated to " + s.version;
        title.textContent = "Updated to " + s.version + ", reloading…";
        items.forEach((li) => { li.className = "done"; });
        bar.style.width = "100%";
        setTimeout(fresh, 1500);
        return;
      }
      if (!s.busy) { // failed, or over: the page says how
        fresh();
        return;
      }
      show(s.state);
    } catch {
      show("restarting…"); // the daemon is between the old and the new binary
    }
    if (Date.now() < giveUp) setTimeout(poll, 1000);
  };
  setTimeout(poll, 1000);
})();
