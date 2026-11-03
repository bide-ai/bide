<script setup lang="ts">
import { approvals } from '../home-data'
import { homeImg } from '../img'
</script>

<template>
  <section class="section">
    <div class="wrap split h-center">
      <div class="h-appr-copy">
        <p class="eyebrow">{{ approvals.eyebrow }}</p>
        <h2 class="h2">{{ approvals.title }}</h2>
        <p class="lede h-mb30">{{ approvals.lede }}</p>
        <ul class="checks">
          <li v-for="c in approvals.checks" :key="c"><span class="k mono">✓</span><span v-html="c"></span></li>
        </ul>
        <p class="small h-mt22">{{ approvals.note }}</p>
      </div>
      <div class="h-appr-img">
        <img
          class="shot"
          :src="homeImg(approvals.image.src)"
          :alt="approvals.image.alt"
          :width="approvals.image.width"
          :height="approvals.image.height"
          loading="lazy"
          decoding="async"
        />
      </div>
    </div>
  </section>
</template>
