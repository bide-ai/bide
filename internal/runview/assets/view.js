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

  const enhanceAll = () => document.querySelectorAll(".graphwrap svg.graph").forEach(enhance);
  window.bideEnhance = enhanceAll; // live.js calls it after it swaps the page's content
  if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", enhanceAll);
  else enhanceAll();
})();
