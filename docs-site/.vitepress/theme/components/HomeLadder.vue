<script setup lang="ts">
import { ladder } from '../home-data'
</script>

<template>
  <section class="section">
    <div class="wrap">
      <p class="eyebrow">{{ ladder.eyebrow }}</p>
      <h2 class="h2 h-narrow">{{ ladder.title }}</h2>
      <p class="lede h-mb48">{{ ladder.lede }}</p>
      <div class="grid g3">
        <div v-for="(s, i) in ladder.steps" :key="s.title" class="card" :class="{ 'card-hot': s.hot }">
          <div class="step-n">{{ i + 1 }}</div>
          <h3 class="h3">{{ s.title }}</h3>
          <p class="muted h-mb18" v-html="s.html"></p>
          <p class="mono h-result" :class="{ hot: s.hot }">{{ s.result }}</p>
        </div>
      </div>
      <p class="h-quote">{{ ladder.quote }}</p>
    </div>
  </section>
</template>
