// Runs assets/theme.js against a fake page and checks what it does: which themes it offers, which
// it applies from storage, what the toggle does. Run by TestThemeJS (node theme_js_test.mjs).
import { readFileSync } from "node:fs";
import vm from "node:vm";
import assert from "node:assert/strict";

const src = readFileSync(new URL("../assets/theme.js", import.meta.url), "utf8");

function page({ themes, def, stored, system = "light", blockedStorage = false, attr = null, noStatus = false, contrast = false, forced = false }) {
  const attrs = new Map(attr ? [["data-theme", attr]] : []);
  const store = new Map(stored ? [["bide-theme", stored]] : []);
  const status = { textContent: "" };
  const root = {
    getAttribute: (k) => (attrs.has(k) ? attrs.get(k) : null),
    setAttribute: (k, v) => attrs.set(k, v),
  };
  const props = { "--bide-themes": themes === undefined ? "" : `"${themes}"`, "--bide-default": def ? `"${def}"` : "" };
  const win = {
    matchMedia: (q) => ({ matches: (q.includes("color-scheme: dark") && system === "dark") || (q.includes("prefers-contrast: more") && contrast) || (q.includes("forced-colors: active") && forced) }),
  };
  const ctx = {
    document: { documentElement: root, getElementById: (id) => (id === "theme-status" && !noStatus ? status : null) },
    getComputedStyle: () => ({ getPropertyValue: (n) => props[n] || "" }),
    localStorage: blockedStorage
      ? { getItem() { throw new Error("blocked"); }, setItem() { throw new Error("blocked"); } }
      : { getItem: (k) => (store.has(k) ? store.get(k) : null), setItem: (k, v) => store.set(k, v) },
    window: win,
  };
  ctx.window.window = ctx.window;
  vm.createContext(ctx);
  // theme.js reads matchMedia through window; give the sandbox the same surface.
  ctx.matchMedia = win.matchMedia;
  vm.runInContext(src, ctx);
  return { attrs, store, status, bide: win.bideTheme };
}

const all = "light dark high-contrast acme-night";

// The list comes from the stylesheet, quotes stripped.
assert.deepEqual(Array.from(page({ themes: all }).bide.names()), ["light", "dark", "high-contrast", "acme-night"]);
// A page whose stylesheet is not loaded has light and dark.
assert.deepEqual(Array.from(page({}).bide.names()), ["light", "dark"]);

// A remembered theme the page has is applied before paint; one it does not have is ignored.
assert.equal(page({ themes: all, stored: "high-contrast" }).attrs.get("data-theme"), "high-contrast");
assert.equal(page({ themes: all, stored: "acme-night" }).attrs.get("data-theme"), "acme-night");
assert.equal(page({ themes: all, stored: "neon" }).attrs.get("data-theme"), undefined);
assert.equal(page({ themes: all, stored: "x\"]{}" }).attrs.get("data-theme"), undefined);
assert.equal(page({ themes: "light dark", stored: "high-contrast" }).attrs.get("data-theme"), undefined);
// Without the list (the editor's page), only light and dark are applied.
assert.equal(page({ stored: "dark" }).attrs.get("data-theme"), "dark");
assert.equal(page({ stored: "high-contrast" }).attrs.get("data-theme"), undefined);
// Blocked storage is not an error and leaves the page as it was.
assert.equal(page({ themes: all, blockedStorage: true }).attrs.get("data-theme"), undefined);

// current: the attribute, else the server's default, else the system.
assert.equal(page({ themes: all, attr: "dark" }).bide.current(), "dark");
assert.equal(page({ themes: all, def: "high-contrast" }).bide.current(), "high-contrast");
assert.equal(page({ themes: all, system: "dark" }).bide.current(), "dark");
assert.equal(page({ themes: all, system: "light" }).bide.current(), "light");
assert.equal(page({ themes: all, def: "dark", system: "light" }).bide.current(), "dark");

// The toggle cycles the list, wraps, and remembers; blocked storage still switches.
let p = page({ themes: all });
assert.deepEqual([p.bide.toggle(), p.bide.toggle(), p.bide.toggle(), p.bide.toggle(), p.bide.toggle()], ["dark", "high-contrast", "acme-night", "light", "dark"]);
assert.equal(p.store.get("bide-theme"), "dark");
assert.equal(p.attrs.get("data-theme"), "dark");
p = page({ themes: all, blockedStorage: true });
assert.equal(p.bide.toggle(), "dark");
assert.equal(p.attrs.get("data-theme"), "dark");
// From the system's dark, the first press goes on to the theme after dark.
assert.equal(page({ themes: all, system: "dark" }).bide.toggle(), "high-contrast");
// An unknown current theme starts the cycle at the first.
assert.equal(page({ themes: all, attr: "gone" }).bide.next(), "light");

// The toggle announces the theme it switched to, and does not fail on a page without the element.
p = page({ themes: all });
p.bide.toggle();
assert.equal(p.status.textContent, "Theme: dark");
p.bide.toggle();
assert.equal(p.status.textContent, "Theme: high-contrast");
assert.equal(page({ themes: all, noStatus: true }).bide.toggle(), "dark");

// A reader's request for more contrast is the high-contrast theme, over the system's dark and over
// a host's default, but not over a choice made in the page, and only where the page has the theme.
assert.equal(page({ themes: all, contrast: true }).bide.current(), "high-contrast");
assert.equal(page({ themes: all, contrast: true, system: "dark" }).bide.current(), "high-contrast");
assert.equal(page({ themes: all, contrast: true, def: "dark" }).bide.current(), "high-contrast");
assert.equal(page({ themes: all, contrast: true, attr: "dark" }).bide.current(), "dark");
assert.equal(page({ themes: "light dark", contrast: true }).bide.current(), "light");
assert.equal(page({ themes: all, contrast: true }).bide.next(), "acme-night");

// Forced colors: the page says so, and the toggle's label can tell the reader.
assert.equal(page({ themes: all, forced: true }).bide.forced(), true);
assert.equal(page({ themes: all }).bide.forced(), false);

console.log("theme.js ok");
