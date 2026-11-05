// The bar at the top of every page: transparent while you're at the top, a compact frosted bar once you scroll down
// (the CSS draws the transition). It turns on past 48px and off under 8px, so its own change of height can't make it
// flicker. Served from this origin; the CSP allows no inline script.
(() => {
  const bar = document.querySelector(".top");
  if (!bar) return;
  let queued = false;
  const update = () => {
    queued = false;
    if (window.scrollY > 48) bar.classList.add("stuck");
    else if (window.scrollY < 8) bar.classList.remove("stuck");
  };
  window.addEventListener("scroll", () => {
    if (!queued) {
      queued = true;
      requestAnimationFrame(update);
    }
  }, { passive: true });
  update(); // a reload can come back scrolled down
})();
