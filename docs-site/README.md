# bide docs site

The [VitePress](https://vitepress.dev) site published at [bide-ai.com](https://bide-ai.com).

It sources its content from the repository's `docs/` tree (via `srcDir: '../docs'`),
so guides live in one place: edit the markdown under `docs/`, not here. This
directory holds only the site shell (config, theme, homepage assets).

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
