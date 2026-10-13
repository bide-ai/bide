// Applies the theme the reader chose, before the page paints. view.js has the toggle; without
// script (or without storage) the page follows prefers-color-scheme.
(() => {
  try {
    const t = localStorage.getItem("bide-theme");
    if (t === "light" || t === "dark") document.documentElement.setAttribute("data-theme", t);
  } catch (e) { /* storage is blocked: the system's theme stays */ }
})();
