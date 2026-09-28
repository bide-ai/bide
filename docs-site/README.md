# bide docs site

The [VitePress](https://vitepress.dev) site published at [bide-ai.com](https://bide-ai.com).

Guides live in one place: edit the markdown under `docs/`, not here. At build
time `sync-docs.mjs` copies `docs/` into this directory (gitignored) so VitePress
resolves modules locally. This directory holds only the site shell (config,
theme, homepage, assets) plus the synced copy.

## Develop

```
cd docs-site
npm install
npm run docs:dev      # local preview at http://localhost:5173
npm run docs:build    # static build to .vitepress/dist
npm run docs:preview  # serve the built site
```

## Deploy

Pushing to `main` builds and deploys to GitHub Pages via
`.github/workflows/docs.yml`. One-time setup:

1. Repo **Settings -> Pages -> Build and deployment -> Source: GitHub Actions**.
2. Set the custom domain to `bide-ai.com` (a `CNAME` file is already emitted from
   `docs/public/CNAME`).
3. Point DNS at GitHub Pages (see the Cloudflare records for `bide-ai.com`).
