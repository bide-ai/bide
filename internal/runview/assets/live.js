// bide view live mode. While the page shows a run in progress, poll /run/live for records after the
// page's cursor; when there are some, fetch the page again and swap in its content, keeping the
// scroll position and open details, and mark the new timeline rows. Stop once the run has its end
// marker (the new page has no live indicator). Reads only; the page is complete without this script.
(() => {
  "use strict";
  const every = 1000, retry = 5000;
  let pill = document.getElementById("live");
  if (!pill || !window.fetch || !window.DOMParser) return;
  const run = encodeURIComponent(pill.dataset.run);
  let cursor = pill.dataset.cursor;
  const main = document.querySelector("main");
  let seen = main.querySelectorAll(".timeline > li").length;
  // Keep the live stylesheet across swaps of the page's content.
  const css = main.querySelector('link[href="/assets/live.css"]');
  if (css) document.head.appendChild(css);

  const get = async (url) => {
    const r = await fetch(url, { cache: "no-store" });
    if (!r.ok) throw new Error(url + ": " + r.status);
    return r;
  };

  const swap = (doc) => {
    const next = doc.querySelector("main");
    if (!next) throw new Error("no page content");
    next.querySelectorAll("script, link").forEach((n) => n.remove());
    // An open <details> is found again by its id (a payload) or else its summary's text.
    const keyOf = (d) => d.id || d.querySelector("summary").textContent;
    const open = new Set([...main.querySelectorAll("details[open]")].map(keyOf));
    const x = window.scrollX, y = window.scrollY;
    main.replaceChildren(...next.childNodes);
    main.querySelectorAll("details").forEach((d) => {
      if (open.has(keyOf(d))) d.open = true;
    });
    if (window.bideEnhance) window.bideEnhance();
    const rows = main.querySelectorAll(".timeline > li");
    for (let i = seen; i < rows.length; i++) rows[i].classList.add("fresh");
    seen = rows.length;
    document.title = doc.title;
    window.scrollTo(x, y);
    pill = document.getElementById("live");
    if (pill) cursor = pill.dataset.cursor;
  };

  const tick = async () => {
    let wait = every;
    if (!document.hidden) {
      try {
        const u = await (await get("/run/live?id=" + run + "&after=" + encodeURIComponent(cursor))).json();
        if (u.changed) {
          const html = await (await get("/run?id=" + run)).text();
          swap(new DOMParser().parseFromString(html, "text/html"));
        }
        if (pill) {
          pill.classList.remove("stale");
          pill.textContent = "live";
        }
      } catch (e) {
        if (pill) {
          pill.classList.add("stale");
          pill.textContent = "live: reconnecting";
        }
        wait = retry;
      }
    }
    if (pill) setTimeout(tick, wait);
  };
  setTimeout(tick, every);
})();
