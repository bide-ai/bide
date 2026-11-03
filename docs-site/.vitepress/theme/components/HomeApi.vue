<script setup lang="ts">
import { api, codeTabs } from '../home-data'
import HomeTabs from './HomeTabs.vue'
</script>

<template>
  <section class="section">
    <div class="wrap split">
      <div class="h-api-copy">
        <p class="eyebrow">{{ api.eyebrow }}</p>
        <h2 class="h2">{{ api.title }}</h2>
        <p class="lede h-mb30">{{ api.lede }}</p>
        <ul class="checks">
          <li v-for="c in api.checks" :key="c"><span class="k mono">✓</span><span v-html="c"></span></li>
        </ul>
      </div>
      <div class="h-api-code">
        <HomeTabs :tabs="codeTabs" label="Code examples" name="code" variant="term">
          <template v-for="t in codeTabs" :key="t.id" #[t.id]>
            <pre class="code" v-html="t.html"></pre>
          </template>
        </HomeTabs>
      </div>
    </div>
  </section>
</template>
