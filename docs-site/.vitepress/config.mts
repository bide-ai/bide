import { defineConfig } from 'vitepress'

// The site shell lives here; page content is sourced from the repo's docs/ tree.
export default defineConfig({
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
      { text: 'Tour', link: '/tour' },
      { text: 'Concepts', link: '/CONCEPTS' },
      { text: 'Recipes', link: '/recipes/' },
      { text: 'Guides', link: '/guides/plan-flows' },
      { text: 'Reference', link: '/reference/cli' },
      { text: 'Proofs', link: '/formal-verification' },
      { text: 'Blog', link: '/blog/' },
    ],
    // The order of the product: start, build, inspect, prove; then the reference and the proofs.
    sidebar: [
      {
        text: 'Start here',
        items: [
          { text: 'Getting started', link: '/getting-started' },
          { text: 'A tour of bide', link: '/tour' },
          { text: 'Concepts', link: '/CONCEPTS' },
          { text: 'The guarantee', link: '/GUARANTEE' },
          { text: 'Known limitations', link: '/KNOWN-LIMITATIONS' },
          { text: 'Changelog', link: '/CHANGELOG' },
        ],
      },
      {
        text: 'Recipes',
        collapsed: false,
        items: [
          { text: 'All recipes', link: '/recipes/' },
          { text: 'Refund with 2-of-3 approval', link: '/recipes/refund-approval' },
          { text: 'Invoice approval', link: '/recipes/invoice-approval' },
          { text: 'Support ticket triage', link: '/recipes/ticket-triage' },
          { text: 'Update a record safely', link: '/recipes/customer-record' },
          { text: 'Ambient agent', link: '/recipes/ambient-agent' },
          { text: 'Get notified', link: '/recipes/notify-human' },
        ],
      },
      {
        text: 'Guides / Build',
        collapsed: false,
        items: [
          { text: 'Plan flows', link: '/guides/plan-flows' },
          { text: 'Durable steps', link: '/guides/durable-steps' },
          { text: 'Reliability', link: '/guides/reliability' },
          { text: 'Signals and ambient', link: '/guides/signals' },
          { text: 'Models', link: '/guides/models' },
          { text: 'MCP', link: '/guides/mcp' },
          { text: 'Messaging', link: '/guides/messaging' },
          { text: 'RAG and memory', link: '/guides/rag-memory' },
          { text: 'Observability', link: '/guides/observability' },
        ],
      },
      {
        text: 'Guides / Inspect',
        collapsed: false,
        items: [
          { text: 'Run viewer', link: '/guides/run-viewer' },
          { text: 'Debugging and recovery', link: '/guides/debugging' },
        ],
      },
      {
        text: 'Guides / Prove and govern',
        collapsed: false,
        items: [
          { text: 'Audit', link: '/guides/audit' },
          { text: 'Human approval (HITL)', link: '/guides/hitl-approval' },
          { text: 'Notifications', link: '/guides/notifications' },
          { text: 'Security model', link: '/guides/security-model' },
          { text: 'Delegation', link: '/guides/delegation' },
          { text: 'Governance', link: '/guides/governance' },
          { text: 'Quorum', link: '/guides/quorum' },
        ],
      },
      {
        text: 'Reference',
        items: [
          { text: 'CLI reference', link: '/reference/cli' },
          { text: 'Extension points', link: '/reference/extension-points' },
          { text: 'Module structure', link: '/reference/module-structure' },
          { text: 'API (pkg.go.dev)', link: 'https://pkg.go.dev/github.com/bide-ai/bide/agent' },
        ],
      },
      {
        text: 'Proofs',
        items: [
          { text: 'Formal verification', link: '/formal-verification' },
          { text: 'How bide is verified', link: '/testing/verification' },
          { text: 'Testing', link: '/testing/testing' },
        ],
      },
    ],
    socialLinks: [{ icon: 'github', link: 'https://github.com/bide-ai/bide' }],
    search: { provider: 'local' },
    editLink: {
      // Pages are synced from docs/, except the changelog (the repo root) and the homepage (this
      // directory).
      pattern: ({ filePath }) =>
        filePath === 'CHANGELOG.md'
          ? 'https://github.com/bide-ai/bide/edit/main/CHANGELOG.md'
          : filePath === 'index.md'
            ? 'https://github.com/bide-ai/bide/edit/main/docs-site/index.md'
            : `https://github.com/bide-ai/bide/edit/main/docs/${filePath}`,
      text: 'Edit this page on GitHub',
    },
    footer: {
      message: 'Apache-2.0 licensed.',
      copyright: 'Copyright 2026 Dayna Blackwell / bide-ai',
    },
  },
})
