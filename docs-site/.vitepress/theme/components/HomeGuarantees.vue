<script setup lang="ts">
import { guarantees } from '../home-data'
</script>

<template>
  <section class="section">
    <div class="wrap">
      <p class="eyebrow">{{ guarantees.eyebrow }}</p>
      <h2 class="h2 h-narrow">{{ guarantees.title }}</h2>
      <div class="grid g-guar">
        <a v-for="c in guarantees.cards" :key="c.n" class="card" :href="c.href">
          <span class="mono h-num">{{ c.n }}<span v-if="c.experimental" class="tag h-tag-gap">experimental</span></span>
          <span class="h-card-title">{{ c.title }}</span>
          <span class="muted">{{ c.text }}</span>
          <ul class="inc">
            <li v-for="b in c.bullets" :key="b">{{ b }}</li>
          </ul>
          <span class="h-more">{{ c.link }}</span>
        </a>
      </div>
    </div>
  </section>
</template>
