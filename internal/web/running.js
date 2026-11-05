// The working lines, the way Claude Code shows it is working: a pulsing glyph and a changing verb, the clock, the stage
// and the steps kept live from /active.json, and a reload when a review finishes (or, on the main page, starts) so it
// moves to where it belongs. The pill in the bar does the same on every page for whatever is running. Served from this
// origin; the CSP allows no inline script.
(() => {
  const rows = [...document.querySelectorAll(".running[data-id]")];
  const pill = document.querySelector("[data-running-pill]");
  const watchAll = document.querySelector("[data-running-all]") !== null; // the main page also shows new ones
  if (!rows.length && !pill && !watchAll) return;

  const calm = window.matchMedia("(prefers-reduced-motion: reduce)").matches;
  const glyphs = ["·", "✢", "✳", "✶", "✻", "✽", "✻", "✶", "✳", "✢"];
  const anyStage = ["Thinking", "Pondering", "Vibing", "Noodling", "Clauding", "Cogitating", "Ruminating", "Percolating",
    "Mulling", "Musing", "Brewing", "Simmering", "Conjuring", "Spelunking", "Wrangling", "Untangling", "Scheming",
    "Finagling", "Moseying", "Puttering", "Marinating", "Contemplating", "Deliberating", "Tinkering"];
  const byStage = [
    [/^(starting|fetching|checking|reading the conversation)/,
      ["Fetching", "Unpacking", "Skimming", "Gathering context", "Cloning", "Reading the room"]],
    [/^reviewing/, ["Reading the diff", "Hunting bugs", "Following the call graph", "Reading ADRs", "Squinting at YAML",
      "Grepping", "Tracing", "Cross-referencing"]],
    [/^verifying/, ["Double-checking", "Cross-examining", "Fact-checking", "Scrutinizing", "Second-guessing"]],
    [/^posting/, ["Writing comments", "Polishing", "Wordsmithing"]],
  ];
  // The step a stage belongs to: the same mapping as reviewStep in view.go.
  const stepOf = (stage) => /^posting/.test(stage) ? 3 : /^verifying/.test(stage) ? 2 : /^reviewing/.test(stage) ? 1 : 0;

  const pick = (list, last) => {
    let v;
    do { v = list[Math.floor(Math.random() * list.length)]; } while (list.length > 1 && v === last);
    return v;
  };
  const verbFor = (stage, last) => {
    const hit = byStage.find(([re]) => re.test(stage));
    return pick(hit && Math.random() < 0.6 ? hit[1] : anyStage, last);
  };
  const clock = (ms) => {
    const s = Math.max(0, Math.floor(ms / 1000)), h = Math.floor(s / 3600), m = Math.floor((s % 3600) / 60);
    return h ? `${h}h ${m}m` : m ? `${m}m ${s % 60}s` : `${s}s`;
  };
  const el = (row, cls) => row.querySelector("." + cls);

  const steps = (id, status, stage) => {
    const list = document.querySelector(`[data-steps-for="${id}"]`);
    if (!list) return;
    const at = status === "running" ? stepOf(stage) : -1;
    [...list.children].forEach((li, i) => { li.className = i < at ? "done" : i === at ? "now" : "todo"; });
  };

  const lines = pill ? [...rows, pill] : rows;
  const state = new Map(lines.map((row) => [row, { verb: "", next: 0 }]));
  let tick = 0;
  const frame = () => {
    const now = Date.now();
    tick++;
    for (const row of lines) {
      const st = state.get(row);
      if (row.dataset.status !== "running") continue;
      if (!calm) el(row, "spin").textContent = glyphs[tick % glyphs.length];
      if (row !== pill && (tick % 8 === 0 || st.next === 0)) {
        const started = Number(row.dataset.started);
        if (started) el(row, "elapsed").textContent = clock(now - started);
      }
      if (now >= st.next) {
        // The pill keeps to the short verbs, so the bar doesn't change width.
        st.verb = row === pill ? pick(anyStage, st.verb) : verbFor(el(row, "stage").textContent, st.verb);
        el(row, "verb").textContent = st.verb + "…";
        st.next = now + (calm ? 8000 : 2500 + Math.random() * 2000);
      }
    }
  };

  // The pill works while any review runs, else says the queue waits.
  const showPill = (active) => {
    if (!pill) return;
    pill.hidden = active.length === 0;
    el(pill, "count-n").textContent = String(active.length);
    const lead = active.find((a) => a.status === "running") || active[0];
    const status = lead ? lead.status : "";
    if (pill.dataset.status !== status) {
      pill.dataset.status = status;
      state.get(pill).next = 0;
      if (status !== "running") {
        el(pill, "spin").textContent = "·";
        el(pill, "verb").textContent = "Waiting in line…";
      }
    }
  };

  const poll = async () => {
    let active;
    try {
      const res = await fetch("/active.json", { cache: "no-store" });
      if (!res.ok) return;
      active = await res.json();
    } catch (_) {
      return; // the daemon is restarting: try again on the next round
    }
    const ids = new Set(active.map((a) => String(a.id)));
    const gone = rows.some((row) => !ids.has(row.dataset.id));
    const added = watchAll && active.some((a) => !rows.some((row) => row.dataset.id === String(a.id)));
    if (gone || added) {
      location.reload();
      return;
    }
    showPill(active);
    for (const a of active) {
      const row = rows.find((r) => r.dataset.id === String(a.id));
      if (!row) continue;
      if (row.dataset.status !== a.status) {
        row.dataset.status = a.status;
        state.get(row).next = 0;
      }
      row.dataset.started = String(a.started);
      el(row, "stage").textContent = a.stage;
      steps(a.id, a.status, a.stage);
    }
  };

  frame();
  setInterval(frame, 120);
  setInterval(poll, 4000);
})();
