<script setup lang="ts">
import { untrusted } from '../home-data'
</script>

<template>
  <section class="section">
    <div class="wrap">
      <p class="eyebrow">{{ untrusted.eyebrow }}</p>
      <h2 class="h2 h-narrow">{{ untrusted.title }}</h2>
      <p class="lede h-mb44">{{ untrusted.lede }}</p>
      <div class="grid g3">
        <a v-for="c in untrusted.cards" :key="c.title" class="card" :href="c.href">
          <span class="tag h-self-start">experimental</span>
          <span class="h-card-title">{{ c.title }}</span>
          <span class="muted" v-html="c.html"></span>
        </a>
      </div>
    </div>
  </section>
</template>
