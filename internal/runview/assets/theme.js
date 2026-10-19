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
  const current = () => root.getAttribute("data-theme") || read("--bide-default") ||
    (window.matchMedia && window.matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light");
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
