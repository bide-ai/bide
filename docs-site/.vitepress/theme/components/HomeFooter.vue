<script setup lang="ts">
import { footer } from '../home-data'
import { withBase } from 'vitepress'
</script>

<template>
  <footer class="h-footer">
    <div class="wrap h-footer-grid">
      <div class="h-footer-brand">
        <span class="h-footer-name"><img :src="withBase('/bide-avatar.png')" alt="" width="24" height="24" />bide</span>
        <span class="small">{{ footer.license }}</span>
        <span class="small">
          README in
          <template v-for="(r, i) in footer.readmes" :key="r.href">
            <a :href="r.href">{{ r.text }}</a><template v-if="i < footer.readmes.length - 1"> · </template>
          </template>
        </span>
      </div>
      <div class="h-footer-cols">
        <div v-for="c in footer.columns" :key="c.title" class="h-footer-col">
          <span class="h-footer-title">{{ c.title }}</span>
          <a v-for="l in c.links" :key="l.href" class="navlink" :href="l.href">{{ l.text }}</a>
        </div>
      </div>
    </div>
  </footer>
</template>
