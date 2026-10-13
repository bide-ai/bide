// bide view page script. The pages are complete without it; it adds only interaction:
//   - copy buttons on a record's JSON payload;
//   - on a flow graph: hover (or focus) a node to highlight every path through it, click it for its
//     details, drag to pan, and zoom with the buttons or ctrl/cmd + wheel.
// It reads the page and changes the page. It makes no request.
(() => {
  "use strict";
  document.documentElement.classList.add("has-js");
  const ns = "http://www.w3.org/2000/svg";

  // --- copy buttons (delegated, so rows live mode swaps in work too)
  document.addEventListener("click", async (ev) => {
    const btn = ev.target.closest && ev.target.closest("button.copy");
    if (!btn) return;
    const text = btn.parentElement.querySelector("pre").textContent;
    try {
      await navigator.clipboard.writeText(text);
    } catch (e) {
      const ta = document.createElement("textarea");
      ta.value = text;
      document.body.appendChild(ta);
      ta.select();
      try { document.execCommand("copy"); } catch (e2) { /* nothing to do: the text stays selectable */ }
      ta.remove();
    }
    const label = btn.lastChild;
    const was = label.textContent;
    btn.classList.add("done");
    label.textContent = " Copied";
    setTimeout(() => { btn.classList.remove("done"); label.textContent = was; }, 1200);
  });

  // --- graphs
  const graphs = new WeakMap();
  const modelOf = (svg) => {
    let m = graphs.get(svg);
    if (m) return m;
    const out = new Map(), inn = new Map();
    const edges = [...svg.querySelectorAll(".edge-g")];
    for (const e of edges) {
      const f = e.dataset.from, t = e.dataset.to;
      if (!out.has(f)) out.set(f, []);
      if (!inn.has(t)) inn.set(t, []);
      out.get(f).push(t);
      inn.get(t).push(f);
    }
    m = { out, inn, edges };
    graphs.set(svg, m);
    return m;
  };
  const reach = (start, adj) => {
    const seen = new Set();
    const todo = [start];
    while (todo.length) {
      for (const n of adj.get(todo.pop()) || []) if (!seen.has(n)) { seen.add(n); todo.push(n); }
    }
    return seen;
  };
  const focus = (node) => {
    const svg = node.closest("svg.graph");
    const id = node.dataset.id;
    const m = modelOf(svg);
    const anc = reach(id, m.inn), desc = reach(id, m.out);
    const keep = new Set([id, ...anc, ...desc]);
    svg.classList.add("focus");
    for (const n of svg.querySelectorAll(".node")) n.classList.toggle("hl", keep.has(n.dataset.id));
    for (const e of m.edges) {
      const f = e.dataset.from, t = e.dataset.to;
      const up = (anc.has(f) && (anc.has(t) || t === id));
      const down = (desc.has(t) && (desc.has(f) || f === id));
      e.classList.toggle("hl", up || down);
    }
  };
  const unfocus = (svg) => {
    svg.classList.remove("focus");
    for (const n of svg.querySelectorAll(".hl")) n.classList.remove("hl");
  };
  const nodeAt = (ev) => ev.target.closest && ev.target.closest(".node");

  document.addEventListener("mouseover", (ev) => { const n = nodeAt(ev); if (n) focus(n); });
  document.addEventListener("mouseout", (ev) => {
    const n = nodeAt(ev);
    if (n && !n.contains(ev.relatedTarget)) unfocus(n.closest("svg.graph"));
  });
  document.addEventListener("focusin", (ev) => { const n = nodeAt(ev); if (n) focus(n); });
  document.addEventListener("focusout", (ev) => { const n = nodeAt(ev); if (n) unfocus(n.closest("svg.graph")); });

  const detail = (svg, node) => {
    const wrap = svg.closest(".graphwrap");
    let box = wrap.nextElementSibling;
    if (!box || !box.classList.contains("gdetail")) {
      box = document.createElement("div");
      box.className = "gdetail";
      box.setAttribute("aria-live", "polite");
      wrap.after(box);
    }
    svg.querySelectorAll(".node.sel").forEach((n) => n.classList.remove("sel"));
    if (!node || box.dataset.id === node.dataset.id) { box.remove(); return; }
    node.classList.add("sel");
    box.dataset.id = node.dataset.id;
    box.replaceChildren();
    const [head, ...rest] = node.dataset.detail.split("\n");
    const h = document.createElement("p");
    h.className = "gname";
    h.textContent = head;
    box.append(h);
    if (rest.length) {
      const p = document.createElement("p");
      p.textContent = rest.join("\n");
      box.append(p);
    }
  };
  document.addEventListener("click", (ev) => {
    const svg = ev.target.closest && ev.target.closest("svg.graph");
    if (!svg || svg.closest(".graphwrap").dataset.moved === "1") return;
    detail(svg, nodeAt(ev));
  });
  document.addEventListener("keydown", (ev) => {
    const n = nodeAt(ev);
    if (n && (ev.key === "Enter" || ev.key === " ")) { ev.preventDefault(); detail(n.closest("svg.graph"), n); }
  });

  // --- pan and zoom: the view box moves; the drawing stays a complete static svg without this
  const enhance = (svg) => {
    if (svg.dataset.enh) return;
    svg.dataset.enh = "1";
    const wrap = svg.closest(".graphwrap");
    const home = svg.getAttribute("viewBox").split(" ").map(Number);
    let vb = home.slice();
    const apply = () => svg.setAttribute("viewBox", vb.join(" "));
    const zoom = (k, cx, cy) => {
      const w = Math.min(Math.max(vb[2] * k, home[2] / 8), home[2] * 2);
      k = w / vb[2];
      const fx = cx === undefined ? 0.5 : cx, fy = cy === undefined ? 0.5 : cy;
      vb = [vb[0] + vb[2] * fx * (1 - k), vb[1] + vb[3] * fy * (1 - k), vb[2] * k, vb[3] * k];
      apply();
      wrap.classList.toggle("pannable", vb[2] < home[2] - 1);
    };
    const tools = document.createElement("div");
    tools.className = "gtools";
    for (const [label, aria, act] of [["+", "Zoom in", () => zoom(0.8)], ["−", "Zoom out", () => zoom(1.25)], ["⟲", "Reset the view", () => { vb = home.slice(); apply(); wrap.classList.remove("pannable"); }]]) {
      const b = document.createElement("button");
      b.type = "button";
      b.textContent = label;
      b.setAttribute("aria-label", aria);
      b.addEventListener("click", act);
      tools.append(b);
    }
    wrap.append(tools);
    svg.addEventListener("wheel", (ev) => {
      if (!ev.ctrlKey && !ev.metaKey) return;
      ev.preventDefault();
      const r = svg.getBoundingClientRect();
      zoom(ev.deltaY > 0 ? 1.12 : 0.89, (ev.clientX - r.left) / r.width, (ev.clientY - r.top) / r.height);
    }, { passive: false });
    let drag = null;
    svg.addEventListener("pointerdown", (ev) => {
      if (ev.button !== 0 || nodeAt(ev) || vb[2] >= home[2] - 1) return;
      drag = { x: ev.clientX, y: ev.clientY, vb: vb.slice() };
      wrap.dataset.moved = "";
      svg.setPointerCapture(ev.pointerId);
      wrap.classList.add("panning");
    });
    svg.addEventListener("pointermove", (ev) => {
      if (!drag) return;
      const r = svg.getBoundingClientRect();
      const dx = ev.clientX - drag.x, dy = ev.clientY - drag.y;
      if (Math.abs(dx) + Math.abs(dy) > 3) wrap.dataset.moved = "1";
      vb[0] = drag.vb[0] - dx * vb[2] / r.width;
      vb[1] = drag.vb[1] - dy * vb[3] / r.height;
      apply();
    });
    const end = () => { drag = null; wrap.classList.remove("panning"); setTimeout(() => { wrap.dataset.moved = ""; }, 0); };
    svg.addEventListener("pointerup", end);
    svg.addEventListener("pointercancel", end);
  };

  // --- the timeline's filter
  const rowsOf = () => [...document.querySelectorAll(".timeline > li")];
  const filterBox = () => {
    const tl = document.querySelector(".timeline");
    if (!tl || document.querySelector(".tfilter")) return;
    const box = document.createElement("div");
    box.className = "tfilter";
    const input = document.createElement("input");
    input.type = "search";
    input.placeholder = "Filter records (press /)";
    input.setAttribute("aria-label", "Filter the timeline's records");
    const count = document.createElement("span");
    count.setAttribute("aria-live", "polite");
    const empty = document.createElement("div");
    empty.className = "empty";
    empty.hidden = true;
    empty.innerHTML = '<p class="etitle">No record matches</p><p>Clear the filter to see all of them.</p>';
    input.addEventListener("input", () => {
      const q = input.value.trim().toLowerCase();
      let shown = 0;
      const rows = rowsOf();
      for (const r of rows) {
        const hit = !q || r.textContent.toLowerCase().includes(q);
        r.hidden = !hit;
        if (hit) shown++;
      }
      count.textContent = q ? shown + " of " + rows.length : "";
      empty.hidden = shown > 0;
    });
    input.addEventListener("keydown", (ev) => { if (ev.key === "Escape") { input.value = ""; input.dispatchEvent(new Event("input")); input.blur(); } });
    box.append(input, count);
    tl.before(box);
    tl.after(empty);
  };

  // --- the theme toggle: light or dark, remembered (storage may be blocked, so every access is guarded)
  const root = document.documentElement;
  const dark = () => {
    const t = root.getAttribute("data-theme");
    return t ? t === "dark" : window.matchMedia && window.matchMedia("(prefers-color-scheme: dark)").matches;
  };
  const toggleTheme = () => {
    const next = dark() ? "light" : "dark";
    root.setAttribute("data-theme", next);
    try { localStorage.setItem("bide-theme", next); } catch (e) { /* not remembered; this page still switched */ }
  };
  document.addEventListener("click", (ev) => { if (ev.target.closest && ev.target.closest("#theme")) toggleTheme(); });

  // --- keyboard: j and k step through the records (or the runs of the list), / filters, ? helps
  const items = () => {
    const rows = rowsOf().filter((r) => !r.hidden);
    return rows.length ? rows : [...document.querySelectorAll("table.runlist tbody tr")];
  };
  const step = (dir) => {
    const list = items();
    if (!list.length) return;
    let i = list.findIndex((r) => r.classList.contains("cur"));
    list.forEach((r) => r.classList.remove("cur"));
    i = i < 0 ? (dir > 0 ? 0 : list.length - 1) : Math.min(Math.max(i + dir, 0), list.length - 1);
    const r = list[i];
    r.classList.add("cur");
    const calm = window.matchMedia && window.matchMedia("(prefers-reduced-motion: reduce)").matches;
    r.scrollIntoView({ block: "center", behavior: calm ? "auto" : "smooth" });
    if (r.id) { try { history.replaceState(null, "", "#" + r.id); } catch (e) { /* the address stays */ } }
  };
  const helpRows = [["j / k", "next / previous record (or run)"], ["Enter", "open the highlighted run"], ["/", "filter"], ["t", "light or dark"], ["s", "share this run as an image"], ["?", "this help"], ["Esc", "close"]];
  const help = (on) => {
    let back = document.querySelector(".helpback");
    if (on === false || (on === undefined && back)) { if (back) back.remove(); return; }
    if (back) return;
    back = document.createElement("div");
    back.className = "helpback";
    const d = document.createElement("div");
    d.className = "help";
    d.setAttribute("role", "dialog");
    d.setAttribute("aria-modal", "true");
    d.setAttribute("aria-label", "Keyboard shortcuts");
    const h = document.createElement("h2");
    h.textContent = "Keyboard shortcuts";
    const dl = document.createElement("dl");
    for (const [k, v] of helpRows) {
      const dt = document.createElement("dt");
      for (const part of k.split(" / ")) { const kb = document.createElement("kbd"); kb.textContent = part; if (dt.childNodes.length) dt.append(" "); dt.append(kb); }
      const dd = document.createElement("dd");
      dd.textContent = v;
      dl.append(dt, dd);
    }
    d.append(h, dl);
    back.append(d);
    back.addEventListener("click", (ev) => { if (ev.target === back) back.remove(); });
    document.body.append(back);
  };
  document.addEventListener("click", (ev) => { if (ev.target.closest && ev.target.closest("#helpbtn")) help(); });
  document.addEventListener("keydown", (ev) => {
    if (ev.ctrlKey || ev.metaKey || ev.altKey) return;
    const t = ev.target;
    const typing = t && (t.isContentEditable || /^(INPUT|TEXTAREA|SELECT)$/.test(t.tagName));
    if (ev.key === "Escape") { help(false); return; }
    if (typing) return;
    switch (ev.key) {
      case "j": step(1); break;
      case "k": step(-1); break;
      case "/": {
        const f = document.querySelector(".tfilter input") || document.querySelector('.filters input[name="q"]');
        if (f) { ev.preventDefault(); f.focus(); f.select(); }
        break;
      }
      case "?": help(); break;
      case "t": toggleTheme(); break;
      case "s": { const b = document.querySelector(".verdict .share"); if (b) b.click(); break; }
      case "Enter": {
        const r = document.querySelector("table.runlist tbody tr.cur a");
        if (r) r.click();
        break;
      }
    }
  });

  const enhanceAll = () => { document.querySelectorAll(".graphwrap svg.graph").forEach(enhance); filterBox(); if (window.bideShareInit) window.bideShareInit(); };
  window.bideEnhance = enhanceAll; // live.js calls it after it swaps the page's content
  if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", enhanceAll);
  else enhanceAll();
})();
