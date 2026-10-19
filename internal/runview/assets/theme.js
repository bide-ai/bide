// Applies the theme the reader chose, before the page paints, and says which themes this page has.
// The stylesheet declares them (--bide-themes, and --bide-default when the server names a start);
// view.js has the toggle. Without script (or without storage) the page follows the server's default
// or, if it has none, prefers-color-scheme.
(() => {
  const root = document.documentElement;
  const read = (name) => getComputedStyle(root).getPropertyValue(name).replace(/["']/g, "").trim();
  // A page whose stylesheet is not loaded yet (the editor's, which loads this script first) has the
  // two themes it always had.
  const names = () => {
    const list = read("--bide-themes").split(/\s+/).filter(Boolean);
    return list.length ? list : ["light", "dark"];
  };
  // The theme in force: the reader's choice, else a reader's request for more contrast (which the
  // stylesheet answers with high-contrast over a host's default), else the server's default, else
  // the system's light or dark.
  const media = (q) => !!(window.matchMedia && window.matchMedia(q).matches);
  const current = () => root.getAttribute("data-theme") ||
    (media("(prefers-contrast: more)") && names().includes("high-contrast") ? "high-contrast" : "") ||
    read("--bide-default") || (media("(prefers-color-scheme: dark)") ? "dark" : "light");
  const next = () => {
    const list = names();
    return list[(list.indexOf(current()) + 1) % list.length];
  };
  // toggle switches to the next theme and remembers it; it returns the name.
  const toggle = () => {
    const n = next();
    root.setAttribute("data-theme", n);
    try { localStorage.setItem("bide-theme", n); } catch (e) { /* not remembered; this page still switched */ }
    // Say it: the button's label changes, which a screen reader does not announce, and the key t
    // may be pressed with the focus elsewhere.
    const say = document.getElementById && document.getElementById("theme-status");
    if (say) say.textContent = "Theme: " + n;
    return n;
  };
  window.bideTheme = { names, current, next, toggle };
  try {
    const t = localStorage.getItem("bide-theme");
    if (t && names().includes(t)) root.setAttribute("data-theme", t);
  } catch (e) { /* storage is blocked: the default or the system's theme stays */ }
})();
