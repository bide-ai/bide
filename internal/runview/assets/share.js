// "Share this run": draws the verdict, the graph (or the proof path) and the proof as a 1200 by 630
// image, in this browser, and saves it as a PNG. The page is turned into an SVG with its computed
// styles written in, the SVG is drawn on a canvas, and the canvas is saved: nothing is sent
// anywhere. The pages are complete without this script.
(() => {
  "use strict";
  const W = 1200, H = 630;
  const esc = (s) => String(s).replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]));
  const css = (el, p) => getComputedStyle(el).getPropertyValue(p).trim();
  const text = (el) => (el ? el.textContent.replace(/\s+/g, " ").trim() : "");

  // wrap breaks s into at most n lines of about max characters, with an ellipsis if it is cut.
  const wrap = (s, max, n) => {
    const lines = [];
    let cur = "";
    for (const w of s.split(" ")) {
      if ((cur + " " + w).trim().length > max && cur) { lines.push(cur); cur = w; } else cur = (cur + " " + w).trim();
    }
    if (cur) lines.push(cur);
    if (lines.length > n) { lines.length = n; lines[n - 1] = lines[n - 1].replace(/.{0,2}$/, "") + "…"; }
    return lines;
  };

  const STYLE = ["fill", "stroke", "stroke-width", "stroke-dasharray", "stroke-linecap", "stroke-linejoin", "opacity", "font-size", "font-weight", "font-family", "font-style", "text-anchor", "text-decoration", "color", "paint-order"];
  // inline returns a copy of the svg with every element's computed style written on it, so it draws
  // the same outside the page's stylesheets.
  const inline = (src) => {
    const copy = src.cloneNode(true);
    const a = [src, ...src.querySelectorAll("*")], b = [copy, ...copy.querySelectorAll("*")];
    a.forEach((el, i) => {
      const cs = getComputedStyle(el);
      b[i].setAttribute("style", STYLE.map((p) => p + ":" + cs.getPropertyValue(p)).join(";"));
      b[i].removeAttribute("class");
    });
    copy.querySelectorAll("title").forEach((t) => t.remove());
    return copy;
  };
  // place draws an inlined svg inside a box, scaled to fit.
  const place = (svg, x, y, w, h) => {
    const copy = inline(svg);
    for (const [k, v] of [["x", x], ["y", y], ["width", w], ["height", h]]) copy.setAttribute(k, v);
    copy.setAttribute("preserveAspectRatio", "xMidYMid meet");
    copy.removeAttribute("role");
    return new XMLSerializer().serializeToString(copy);
  };
  const glyph = (name, x, y, size, color) => {
    const sym = document.getElementById("ic-" + name);
    if (!sym) return "";
    return `<svg x="${x}" y="${y}" width="${size}" height="${size}" viewBox="0 0 16 16" style="color:${color}">${sym.innerHTML}</svg>`;
  };

  const build = () => {
    const body = document.body, v = document.querySelector(".verdict");
    const bg = css(body, "background-color"), fg = css(body, "color");
    const card = css(document.documentElement, "--card"), line = css(document.documentElement, "--line");
    const muted = css(document.documentElement, "--muted"), good = css(document.documentElement, "--good");
    const vc = css(v, "--vc"), vbg = css(v, "--vbg");
    const font = css(body, "font-family"), mono = css(document.documentElement, "--mono");
    const word = text(v.querySelector(".vword")), vline = text(v.querySelector(".vline"));
    const run = text(document.querySelector("h1.rid"));
    const icon = (v.querySelector(".vicon svg") || {}).classList ? [...v.querySelector(".vicon svg").classList].find((c) => c.startsWith("ic-")) : "";
    const stats = [...document.querySelectorAll(".stat")].slice(0, 4).map((s) => [text(s.querySelector(".num")), text(s.querySelector(".lbl")), text(s.querySelector(".sub"))]);
    const proof = [...document.querySelectorAll(".card")].find((c) => text(c.querySelector("h3")) === "Audit proof");
    const visual = document.querySelector(".graphwrap svg.graph") || document.querySelector(".graphwrap svg.merkle");
    const approvers = [...document.querySelectorAll(".acard")].slice(0, 6);
    const rightTitle = !visual ? "Approvers" : visual.classList.contains("graph") ? "Flow" : "Proof path";
    const hasRight = visual || approvers.length;
    const lw = hasRight ? 550 : 1120;

    let o = `<svg xmlns="http://www.w3.org/2000/svg" width="${W}" height="${H}" viewBox="0 0 ${W} ${H}" font-family='${font.replace(/'/g, "")}'>`;
    o += `<rect width="${W}" height="${H}" fill="${bg}"/>`;
    o += `<text x="40" y="50" font-size="22" font-weight="800" fill="${fg}">bide view</text>`;
    o += `<text x="${W - 40}" y="50" font-size="20" font-weight="700" fill="${muted}" text-anchor="end" font-family="${mono.replace(/"/g, "")}">${esc(run.length > 44 ? run.slice(0, 43) + "…" : run)}</text>`;
    // the verdict
    const fs = Math.min(48, Math.floor((lw - 120) / (word.length * 0.66)));
    const lines = wrap(vline, hasRight ? 42 : 90, hasRight ? 4 : 2);
    const vh = hasRight ? 164 : 150;
    o += `<rect x="40" y="76" width="${lw}" height="${vh}" rx="16" fill="${vbg}" stroke="${vc}" stroke-width="2"/><rect x="40" y="76" width="9" height="${vh}" rx="4" fill="${vc}"/>`;
    o += glyph(icon.replace("ic-", ""), 66, 96, 44, vc);
    o += `<text x="124" y="134" font-size="${fs}" font-weight="800" fill="${vc}">${esc(word)}</text>`;
    lines.forEach((l, i) => { o += `<text x="66" y="${178 + i * 27}" font-size="21" fill="${fg}">${esc(l)}</text>`; });
    // the numbers
    const cols = hasRight ? 2 : 4, sw = hasRight ? 265 : 272, sh = hasRight ? 128 : 92, gx = 20, y0 = 76 + vh + 18;
    stats.forEach((s, i) => {
      const x = 40 + (i % cols) * (sw + gx), y = y0 + Math.floor(i / cols) * (sh + 14);
      o += `<rect x="${x}" y="${y}" width="${sw}" height="${sh}" rx="12" fill="${card}" stroke="${line}"/>`;
      o += `<text x="${x + 16}" y="${y + (hasRight ? 58 : 42)}" font-size="${hasRight ? 46 : 38}" font-weight="800" fill="${fg}">${esc(s[0])}</text>`;
      o += `<text x="${x + 16}" y="${y + (hasRight ? 86 : 64)}" font-size="${hasRight ? 14 : 13}" font-weight="700" fill="${muted}" letter-spacing="1">${esc(s[1].toUpperCase())}</text>`;
      if (s[2]) o += `<text x="${x + 16}" y="${y + (hasRight ? 108 : 82)}" font-size="${hasRight ? 14 : 13}" fill="${muted}">${esc(s[2].length > 34 ? s[2].slice(0, 33) + "…" : s[2])}</text>`;
    });
    // the proof, along the foot of the left column
    if (!hasRight) {
    const py = H - 94;
    o += `<rect x="40" y="${py}" width="${lw}" height="64" rx="12" fill="${card}" stroke="${line}"/>`;
    const pv = proof ? text(proof.querySelector(".cval")) : "No audit proof given";
    const pl = proof ? text(proof.querySelector(".cline")) : "";
    const ok = /^\s*Verified/.test(pv);
    o += glyph(ok ? "approval" : "idle", 56, py + 16, 32, ok ? good : muted);
    o += `<text x="100" y="${py + 28}" font-size="18" font-weight="700" fill="${ok ? good : fg}">${esc(pv)}</text>`;
    o += `<text x="100" y="${py + 50}" font-size="14" fill="${muted}">${esc(pl.length > 70 ? pl.slice(0, 69) + "…" : pl)}</text>`;
    }
    // the graph, the proof path, or the approvers, on the right
    if (hasRight) {
      o += `<rect x="610" y="76" width="550" height="${H - 76 - 36}" rx="16" fill="${card}" stroke="${line}"/>`;
      o += `<text x="630" y="104" font-size="13" font-weight="700" fill="${muted}" letter-spacing="1.2">${esc(rightTitle.toUpperCase())}</text>`;
      const merkle = visual && visual.classList.contains("merkle");
      if (visual) o += place(visual, 626, 116, 518, merkle && approvers.length ? 296 : H - 76 - 36 - 52);
      const who = (a) => a.className.includes("approved") ? good : a.className.includes("denied") ? css(document.documentElement, "--bad") : muted;
      if (!visual) {
        approvers.forEach((a, i) => {
          const y = 124 + i * 66, st = who(a);
          o += `<circle cx="660" cy="${y + 22}" r="22" fill="none" stroke="${st}" stroke-width="3"/><text x="660" y="${y + 29}" font-size="18" font-weight="800" fill="${st}" text-anchor="middle">${esc(text(a.querySelector(".avatar")))}</text>`;
          o += `<text x="700" y="${y + 20}" font-size="19" font-weight="700" fill="${fg}">${esc(text(a.querySelector(".aname")))}</text>`;
          o += `<text x="700" y="${y + 42}" font-size="14" fill="${muted}">${esc(text(a.querySelector(".averify")))}</text>`;
        });
      } else if (merkle && approvers.length) {
        o += `<line x1="630" y1="430" x2="1140" y2="430" stroke="${line}"/><text x="630" y="458" font-size="13" font-weight="700" fill="${muted}" letter-spacing="1.2">APPROVERS</text>`;
        approvers.slice(0, 3).forEach((a, i) => {
          const x = 630 + i * 172, st = who(a);
          o += `<circle cx="${x + 20}" cy="504" r="20" fill="none" stroke="${st}" stroke-width="3"/><text x="${x + 20}" y="510" font-size="16" font-weight="800" fill="${st}" text-anchor="middle">${esc(text(a.querySelector(".avatar")))}</text>`;
          o += `<text x="${x + 48}" y="500" font-size="16" font-weight="700" fill="${fg}">${esc(text(a.querySelector(".aname b")))}</text>`;
          const vv = text(a.querySelector(".averify")) || text(a.querySelector(".astate"));
          o += `<text x="${x + 48}" y="520" font-size="12.5" fill="${muted}">${esc(vv.length > 20 ? vv.slice(0, 19) + "…" : vv)}</text>`;
        });
      }
    }
    o += `<text x="40" y="${H - 12}" font-size="13" fill="${muted}">bide view · github.com/bide-ai/bide</text>`;
    return o + "</svg>";
  };

  const save = (btn) => {
    const label = btn.lastChild;
    const done = (msg) => { btn.removeAttribute("aria-busy"); label.textContent = " " + msg; setTimeout(() => { label.textContent = " Share this run"; }, 2200); };
    btn.setAttribute("aria-busy", "true");
    label.textContent = " Rendering…";
    let svg;
    try { svg = build(); } catch (e) { done("Could not draw the card"); return; }
    const img = new Image();
    img.onload = () => {
      try {
        const c = document.createElement("canvas");
        c.width = W; c.height = H;
        c.getContext("2d").drawImage(img, 0, 0, W, H);
        c.toBlob((blob) => {
          if (!blob) { done("Could not save the image"); return; }
          const a = document.createElement("a");
          a.href = URL.createObjectURL(blob);
          a.download = "bide-" + (text(document.querySelector("h1.rid")).replace(/[^\w.-]+/g, "_") || "run") + ".png";
          document.body.append(a);
          a.click();
          a.remove();
          setTimeout(() => URL.revokeObjectURL(a.href), 4000);
          done("Saved the image");
        }, "image/png");
      } catch (e) { done("Could not save the image"); }
    };
    img.onerror = () => done("Could not draw the card");
    img.src = "data:image/svg+xml;charset=utf-8," + encodeURIComponent(svg);
  };

  const init = () => {
    if (location.pathname !== "/run") return;
    const v = document.querySelector(".verdict");
    if (!v || v.querySelector(".share")) return;
    const b = document.createElement("button");
    b.type = "button";
    b.className = "share";
    b.title = "Save a 1200 by 630 image of the verdict, graph and proof (s)";
    b.innerHTML = '<svg class="ic" viewBox="0 0 16 16" aria-hidden="true"><use href="#ic-share"/></svg> Share this run';
    b.addEventListener("click", () => save(b));
    v.append(b);
  };
  window.bideShareInit = init;
  window.bideBuildShareCard = build; // for tests and for anyone who wants the SVG
  if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", init);
  else init();
})();
