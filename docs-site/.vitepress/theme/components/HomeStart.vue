<script setup lang="ts">
import { start } from '../home-data'
</script>

<template>
  <section class="section">
    <div class="wrap">
      <p class="eyebrow">{{ start.eyebrow }}</p>
      <h2 class="h2">{{ start.title }}</h2>
      <p class="lede h-mb44">{{ start.lede }}</p>
      <div class="grid g3 h-mb72">
        <div v-for="s in start.steps" :key="s.label" class="card h-step-card">
          <span class="mono h-step-label">{{ s.label }}</span>
          <span class="h-step-text">{{ s.text }}</span>
          <code class="cmd">{{ s.cmd }}</code>
        </div>
      </div>
      <div class="h-head-row">
        <h3 class="h-sub">{{ start.recipesTitle }}</h3>
        <a class="h-strong-link" href="/recipes/">{{ start.recipesLink }}</a>
      </div>
      <div class="grid g-recipes">
        <a v-for="r in start.recipes" :key="r.href" class="card card-tight" :href="r.href">
          <span class="h-recipe-title">{{ r.title }}</span>
          <span class="muted h-recipe-text">{{ r.text }}</span>
        </a>
      </div>
      <p class="small h-mt20">
        {{ start.examplesText }} <a :href="start.examplesHref">{{ start.examplesLink }}</a>
      </p>
    </div>
  </section>
</template>
