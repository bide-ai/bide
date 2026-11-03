<script setup lang="ts">
import { proofs } from '../home-data'
</script>

<template>
  <section class="section">
    <div class="wrap">
      <p class="eyebrow">{{ proofs.eyebrow }}</p>
      <h2 class="h2 h-narrow">{{ proofs.title }}</h2>
      <p class="lede h-mb40">{{ proofs.lede }}</p>
      <div class="grid g-stats">
        <div v-for="s in proofs.stats" :key="s.value + s.html" class="stat">
          <b>{{ s.value }}</b><span v-html="s.html"></span>
        </div>
      </div>
      <div class="grid g4">
        <a v-for="c in proofs.cards" :key="c.title" class="card" :href="c.href">
          <span class="mono h-proof-title">{{ c.title }} <span v-if="c.experimental" class="tag">experimental</span></span>
          <span class="muted h-proof-text">{{ c.text }}</span>
        </a>
      </div>
      <p class="small h-mt20">
        {{ proofs.note }}
        <template v-for="(l, i) in proofs.links" :key="l.href">
          <a :href="l.href">{{ l.text }}</a><template v-if="i < proofs.links.length - 1"> · </template>
        </template>
      </p>
    </div>
  </section>
</template>
