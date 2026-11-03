<script setup lang="ts">
import { benchmark, problem } from '../home-data'
</script>

<template>
  <section class="section">
    <div class="wrap h-problem">
      <div class="h-problem-copy">
        <p class="eyebrow">{{ problem.eyebrow }}</p>
        <h2 class="h2">{{ problem.title }}</h2>
        <p class="lede h-mb22">{{ problem.p1 }}</p>
        <p class="lede h-mb28">{{ problem.p2 }}</p>
        <a class="h-strong-link" :href="problem.linkHref">{{ problem.linkText }}</a>
      </div>
      <div class="h-problem-rows">
        <div v-for="r in benchmark" :key="r.name" class="row" :class="{ 'row-us': r.us }">
          <div>
            <div class="name">{{ r.name }}</div>
            <div class="meta mono">{{ r.meta }}</div>
          </div>
          <div>
            <div class="verdict">{{ r.verdict }}</div>
            <div class="worst">{{ r.worst }}</div>
          </div>
        </div>
        <p class="small h-bench-note">{{ problem.note }}</p>
      </div>
    </div>
  </section>
</template>
