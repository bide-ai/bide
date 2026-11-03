<script setup lang="ts">
import { untrusted } from '../home-data'
import { homeImg } from '../img'
import HomeTabs from './HomeTabs.vue'
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
      <div class="h-untrusted-shots">
        <HomeTabs :tabs="untrusted.shots" label="Safety controls in bide view" name="untrusted" variant="plain">
          <template v-for="s in untrusted.shots" :key="s.id" #[s.id]>
            <figure class="h-untrusted-fig">
              <div class="frame">
                <img
                  :src="homeImg(s.image.src)"
                  :alt="s.image.alt"
                  :width="s.image.width"
                  :height="s.image.height"
                  :style="{ objectPosition: s.image.position }"
                  loading="lazy"
                  decoding="async"
                />
              </div>
              <figcaption class="small">{{ s.caption }}</figcaption>
            </figure>
          </template>
        </HomeTabs>
      </div>
    </div>
  </section>
</template>
