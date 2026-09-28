import { defineConfig } from 'vitepress'

// The site shell lives here; page content is sourced from the repo's docs/ tree.
export default defineConfig({
  srcDir: '../docs',
  srcExclude: ['README.md', 'CONTEXT.md', 'design/**'],
  cleanUrls: true,
  // The docs are authored to be read in-repo on GitHub too, so they link to
  // source files, examples, and CONTRIBUTING with repo-relative paths. Those
  // targets are not pages on the standalone site, so skip the dead-link gate
  // rather than fork the prose. (Follow-up: rewrite source links to absolute
  // github.com URLs so they resolve on the site as well.)
  ignoreDeadLinks: true,
  lang: 'en-US',
  title: 'bide',
  description: 'Build durable AI agents in Go. Side effects that fire at most once.',
  sitemap: { hostname: 'https://bide-ai.com' },
  head: [
    ['link', { rel: 'icon', href: '/favicon.png' }],
    ['meta', { name: 'theme-color', content: '#E8A33D' }],
    ['meta', { property: 'og:type', content: 'website' }],
    ['meta', { property: 'og:title', content: 'bide' }],
    ['meta', { property: 'og:description', content: 'Build durable AI agents in Go. Side effects that fire at most once.' }],
    ['meta', { property: 'og:url', content: 'https://bide-ai.com' }],
    ['meta', { property: 'og:image', content: 'https://bide-ai.com/bide-social.png' }],
    ['meta', { name: 'twitter:card', content: 'summary_large_image' }],
  ],
  themeConfig: {
    logo: '/favicon.png',
    siteTitle: 'bide',
    nav: [
      { text: 'Get started', link: '/getting-started' },
      { text: 'Concepts', link: '/CONCEPTS' },
      { text: 'Guides', link: '/guides/flows' },
      { text: 'Reference', link: '/reference/module-structure' },
    ],
    sidebar: [
      {
        text: 'Start here',
        items: [
          { text: 'Getting started', link: '/getting-started' },
          { text: 'Concepts', link: '/CONCEPTS' },
          { text: 'The guarantee', link: '/GUARANTEE' },
          { text: 'Known limitations', link: '/KNOWN-LIMITATIONS' },
        ],
      },
      {
        text: 'Guides / Authoring',
        collapsed: false,
        items: [
          { text: 'Reliability', link: '/guides/reliability' },
          { text: 'Durable steps', link: '/guides/durable-steps' },
          { text: 'Flows', link: '/guides/flows' },
          { text: 'Signals and ambient', link: '/guides/signals' },
          { text: 'Observability', link: '/guides/observability' },
          { text: 'Models', link: '/guides/models' },
          { text: 'MCP', link: '/guides/mcp' },
          { text: 'Messaging', link: '/guides/messaging' },
          { text: 'RAG and memory', link: '/guides/rag-memory' },
          { text: 'Debugging and recovery', link: '/guides/debugging' },
        ],
      },
      {
        text: 'Guides / Accountability',
        collapsed: false,
        items: [
          { text: 'Audit', link: '/guides/audit' },
          { text: 'Delegation', link: '/guides/delegation' },
          { text: 'Security model', link: '/guides/security-model' },
          { text: 'Governance', link: '/guides/governance' },
          { text: 'Quorum', link: '/guides/quorum' },
        ],
      },
      {
        text: 'Reference',
        items: [
          { text: 'Extension points', link: '/reference/extension-points' },
          { text: 'Module structure', link: '/reference/module-structure' },
          { text: 'Testing', link: '/testing/testing' },
        ],
      },
    ],
    socialLinks: [{ icon: 'github', link: 'https://github.com/bide-ai/bide' }],
    search: { provider: 'local' },
    editLink: {
      pattern: 'https://github.com/bide-ai/bide/edit/main/docs/:path',
      text: 'Edit this page on GitHub',
    },
    footer: {
      message: 'Apache-2.0 licensed.',
      copyright: 'Copyright 2026 Dayna Blackwell / Blackwell Systems',
    },
  },
})
