// The Autonomy page: reads its numbers from the data-autonomy attribute (CSP blocks inline scripts), fills the DOM,
// runs the assemble animation, and remembers the light/dark choice.
(function () {
  const root = document.getElementById("autonomy");
  if (!root) return;
  let D;
  try { D = JSON.parse(root.dataset.autonomy); } catch (e) { return; }
  const reduce = matchMedia("(prefers-reduced-motion: reduce)").matches;
  const ease = (t) => 1 - Math.pow(1 - t, 3);
  const g = (id) => document.getElementById(id);

  // theme: dark is the default; the choice is remembered
  try { if (localStorage.getItem("prb-autonomy-theme") === "light") document.documentElement.setAttribute("data-theme", "light"); } catch (e) {}
  const toggle = g("toggle");
  if (toggle) toggle.addEventListener("click", () => {
    const light = document.documentElement.getAttribute("data-theme") === "light";
    if (light) document.documentElement.removeAttribute("data-theme"); else document.documentElement.setAttribute("data-theme", "light");
    try { localStorage.setItem("prb-autonomy-theme", light ? "dark" : "light"); } catch (e) {}
  });

  g("carried").textContent = D.carried;
  g("total").textContent = D.reviews;
  g("needed").textContent = D.needed;
  g("target").textContent = D.target + "%";

  const lane = g("lane"), pills = [];
  for (let i = 0; i < D.reviews; i++) {
    const p = document.createElement("div");
    p.className = "pill" + (i < D.carried ? " carried" : "");
    lane.appendChild(p); pills.push(p);
  }
  g("labels").innerHTML = "";
  (D.mix || []).forEach((m) => {
    const s = document.createElement("span");
    s.className = "label " + m.c; s.textContent = m.k;
    g("labels").appendChild(s);
  });

  const up = (el, to, dur, fmt) => {
    if (reduce) { el.textContent = fmt(to); return; }
    const t0 = performance.now();
    (function s(t) { const k = Math.min(1, (t - t0) / dur); el.textContent = fmt(to * ease(k)); if (k < 1) requestAnimationFrame(s); })(t0);
  };
  const f1 = (v) => v.toFixed(1), fi = (v) => Math.round(v) + "", fp = (v) => Math.round(v) + "%";

  requestAnimationFrame(() => {
    pills.forEach((p, i) => setTimeout(() => p.classList.add("in"), reduce ? 0 : 120 + i * 55));
    up(g("hrs"), D.hours, 950, f1);
    setTimeout(() => { g("done").style.width = D.floor + "%"; g("tgt").style.left = D.target + "%"; }, reduce ? 0 : 300);
    up(g("floor"), D.floor, 1000, fp);
    setTimeout(() => { up(g("verb"), D.verbatim, 650, fp); up(g("sent"), D.sentAsYou, 650, fi); up(g("push"), D.disputed, 650, fi); }, reduce ? 0 : 1000);
  });
})();
