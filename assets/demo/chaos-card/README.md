# Chaos results card

`results.json` holds the numbers on `assets/chaos-results.png` (the README card); `card.html` is
the template; `build.mjs` renders it with headless Chrome and writes an optimized palette PNG.

```sh
(cd assets/demo/browser-shots && npm ci --ignore-scripts)
CHROME_BIN=/path/to/chrome node assets/demo/chaos-card/build.mjs      # writes assets/chaos-results.png
```

To refresh: run the benchmarks (`benchmarks/README.md`), copy the rows, versions, date and bide's
commit into `results.json`, run the build, and update every doc that states them. Needs Node 20+
and python3 with Pillow. Fonts are the system stack, so a different machine may differ slightly
in letter shapes.
