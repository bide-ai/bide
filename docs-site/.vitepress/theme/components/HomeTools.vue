<script setup lang="ts">
import { tools } from '../home-data'
import { homeImg } from '../img'
import HomeTabs from './HomeTabs.vue'
</script>

<template>
  <section class="section">
    <div class="wrap">
      <p class="eyebrow">{{ tools.eyebrow }}</p>
      <h2 class="h2">{{ tools.title }}</h2>
      <p class="lede h-mb36" v-html="tools.ledeHtml"></p>
      <HomeTabs :tabs="tools.tabs" label="bide tools" name="tools" variant="plain">
        <template v-for="t in tools.tabs" :key="t.id" #[t.id]>
          <div class="split h-tool">
            <div class="h-tool-copy">
              <h3 class="h-tool-title">{{ t.title }} <span v-if="t.experimental" class="tag">experimental</span></h3>
              <p class="muted h-tool-text" v-html="t.html"></p>
              <code class="cmd h-mb18">{{ t.cmd }}</code>
              <a class="h-strong-link" :href="t.href">{{ t.linkText }}</a>
            </div>
            <div class="h-tool-img" :class="{ stack: t.images.length > 1 }">
              <template v-for="im in t.images" :key="im.src">
                <div v-if="im.mode === 'frame'" class="frame">
                  <img :src="homeImg(im.src)" :alt="im.alt" :width="im.width" :height="im.height" loading="lazy" decoding="async" />
                </div>
                <img v-else class="shot" :src="homeImg(im.src)" :alt="im.alt" :width="im.width" :height="im.height" loading="lazy" decoding="async" />
              </template>
            </div>
          </div>
        </template>
      </HomeTabs>
    </div>
  </section>
</template>
