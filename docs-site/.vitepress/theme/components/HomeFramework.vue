<script setup lang="ts">
import { framework } from '../home-data'
</script>

<template>
  <section class="section">
    <div class="wrap">
      <p class="eyebrow">{{ framework.eyebrow }}</p>
      <h2 class="h2 h-narrow">{{ framework.title }}</h2>
      <p class="lede h-mb44">{{ framework.lede }}</p>
      <div class="grid g3">
        <div v-for="c in framework.cards" :key="c.title" class="card">
          <h3 class="h3">{{ c.title }}</h3>
          <p class="muted h-flush" v-html="c.html"></p>
        </div>
      </div>
      <div class="grid g2 h-mt16">
        <div v-for="c in framework.callouts" :key="c" class="callout" v-html="c"></div>
      </div>
    </div>
  </section>
</template>
