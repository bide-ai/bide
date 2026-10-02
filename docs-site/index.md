---
layout: home

hero:
  name: bide
  text: Build durable AI agents in Go
  tagline: A full agentic framework on one append-only journal. Side effects that fire at most once.
  image:
    src: /bide-avatar.png
    alt: bide
  actions:
    - theme: brand
      text: Get started
      link: /getting-started
    - theme: alt
      text: Concepts
      link: /CONCEPTS
    - theme: alt
      text: GitHub
      link: https://github.com/bide-ai/bide

features:
  - title: At most once
    details: A resumed run never re-charges a card or re-sends an email. Side effects are recorded on the journal, not replayed on recovery.
  - title: HA in one process
    details: Thousands of concurrent durable runs survive crashes and node handoffs. No cluster, no separate workflow engine to operate.
  - title: Verifiable audit trail
    details: Every run's journal is committed to an RFC 6962 Merkle tree. Build inclusion and consistency proofs for any record on demand and check them offline, without trusting the vendor or the process that produced them.
  - title: Provably convergent state
    details: Shared governed state is certified against a machine-checked convergence theorem, not eventual hope. gsm v0.11.0 has a known Build gap; see Known limitations.
  - title: The whole agent surface
    details: Models (OpenAI, Anthropic, Gemini), tools, typed multi-step flows, memory and RAG, MCP, multi-agent coordination, and typed human-in-the-loop.
  - title: A library, not a cluster
    details: Plain Go. Import it, bring your own store. Built for ambient agents that run unattended and act under audit.
---
