// The Autonomy page content: reads its numbers from the #au-root data attribute (CSP blocks inline scripts), fills the
// DOM, and runs the assemble animation. Lives inside the tool's normal page layout.
(function () {
  const root = document.getElementById("au-root");
  if (!root) return;
  let D;
  try { D = JSON.parse(root.dataset.autonomy); } catch (e) { return; }
  const reduce = matchMedia("(prefers-reduced-motion: reduce)").matches;
  const ease = (t) => 1 - Math.pow(1 - t, 3);
  const g = (id) => document.getElementById(id);

  g("au-carried").textContent = D.carried;
  g("au-total").textContent = D.reviews;
  g("au-needed").textContent = D.needed;
  g("au-target").textContent = D.target + "%";

  const lane = g("au-lane"), pills = [];
  for (let i = 0; i < D.reviews; i++) {
    const p = document.createElement("div");
    p.className = "au-pill" + (i < D.carried ? " carried" : "");
    lane.appendChild(p); pills.push(p);
  }
  const labels = g("au-labels"); labels.innerHTML = "";
  (D.mix || []).forEach((m) => {
    const s = document.createElement("span");
    s.className = "au-label " + m.c; s.textContent = m.k;
    labels.appendChild(s);
  });

  const up = (el, to, dur, fmt) => {
    if (reduce) { el.textContent = fmt(to); return; }
    const t0 = performance.now();
    (function s(t) { const k = Math.min(1, (t - t0) / dur); el.textContent = fmt(to * ease(k)); if (k < 1) requestAnimationFrame(s); })(t0);
  };
  const f1 = (v) => v.toFixed(1), fi = (v) => Math.round(v) + "", fp = (v) => Math.round(v) + "%";

  requestAnimationFrame(() => {
    pills.forEach((p, i) => setTimeout(() => p.classList.add("in"), reduce ? 0 : 120 + i * 55));
    up(g("au-hrs"), D.hours, 950, f1);
    setTimeout(() => { g("au-done").style.width = D.floor + "%"; g("au-tgt").style.left = D.target + "%"; }, reduce ? 0 : 300);
    up(g("au-floor"), D.floor, 1000, fp);
    setTimeout(() => { up(g("au-verb"), D.verbatim, 650, fp); up(g("au-sent"), D.sentAsYou, 650, fi); up(g("au-push"), D.disputed, 650, fi); }, reduce ? 0 : 1000);
  });
})();
