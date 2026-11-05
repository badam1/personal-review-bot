// The reviewer filter on a review's page: show one reviewer's findings, or all of them. Without JavaScript it stays
// hidden and every finding shows. Served from this origin; the CSP allows no inline script.
(() => {
  const bar = document.getElementById("filter");
  if (!bar) return;
  const buttons = [...bar.querySelectorAll("button[data-agent]")];
  const rows = [...document.querySelectorAll("[data-finding][data-agent]")];
  const sections = [...document.querySelectorAll("[data-filtered]")];

  const show = (agent) => {
    buttons.forEach((b) => b.setAttribute("aria-pressed", String(b.dataset.agent === agent)));
    rows.forEach((r) => { r.hidden = agent !== "" && r.dataset.agent !== agent; });
    sections.forEach((s) => { s.hidden = !s.querySelector("[data-finding]:not([hidden])"); });
    document.dispatchEvent(new Event("prb:filter")); // Select all counts only what it can see
  };
  buttons.forEach((b) => b.addEventListener("click", () => show(b.dataset.agent)));
  bar.hidden = false;
})();
