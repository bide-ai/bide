<script setup lang="ts">
import { data } from '../version.data'
import { blog, cta, status } from '../home-data'

const statusText = status.statusText.replace('{releases}', String(data.releases)).replace('{since}', data.since)
</script>

<template>
  <section class="section">
    <div class="wrap">
      <div class="h-head-row">
        <h3 class="h-sub">{{ blog.title }}</h3>
        <a class="h-strong-link" href="/blog/">{{ blog.link }}</a>
      </div>
      <div class="grid g2">
        <a v-for="p in blog.posts" :key="p.href" class="card" :href="p.href">
          <span class="mono h-date">{{ p.date }}</span>
          <span class="h-post-title">{{ p.title }}</span>
          <span class="muted">{{ p.text }}</span>
        </a>
      </div>
    </div>
  </section>

  <section class="section h-status">
    <div class="wrap grid g2 h-top">
      <div>
        <p class="eyebrow">{{ status.statusTitle }}</p>
        <p class="muted h-status-text">{{ statusText }}</p>
      </div>
      <div>
        <p class="eyebrow">{{ status.talkTitle }}</p>
        <p class="muted h-status-text">
          {{ status.talkText }} <span class="mono h-email">{{ status.email }}</span>
        </p>
      </div>
    </div>
  </section>

  <section class="h-cta">
    <div class="wrap">
      <h2 class="h2 h-cta-title">{{ cta.title }}</h2>
      <div class="term h-cta-term"><pre class="code" v-html="cta.cmdHtml"></pre></div>
      <div class="h-actions h-actions-center">
        <a class="btn btn-p" href="/getting-started">Get started</a>
        <a class="btn btn-s" href="https://github.com/bide-ai/bide">View on GitHub</a>
      </div>
    </div>
  </section>
</template>
