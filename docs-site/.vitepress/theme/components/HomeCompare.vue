<script setup lang="ts">
import { compare } from '../home-data'
</script>

<template>
  <section class="section">
    <div class="wrap">
      <p class="eyebrow">{{ compare.eyebrow }}</p>
      <h2 class="h2">{{ compare.title }}</h2>
      <div class="h-table-wrap" tabindex="0" role="region" aria-label="Comparison table, scrolls sideways on narrow screens">
        <table class="tbl">
          <thead>
            <tr>
              <th v-for="(c, i) in compare.columns" :key="i" scope="col" :class="{ 'th-us': i === 1 }">{{ c }}</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="r in compare.rows" :key="r[0]">
              <td v-for="(c, i) in r" :key="i" :class="i === 0 ? 'b' : i === 1 ? 'us' : ''" v-html="c"></td>
            </tr>
          </tbody>
        </table>
      </div>
      <div class="grid g3 h-mt20">
        <div v-for="c in compare.callouts" :key="c" class="callout" v-html="c"></div>
      </div>
      <p class="small h-mt16">
        {{ compare.sourcesText }} <a :href="compare.sourcesHref">{{ compare.sourcesLink }}</a>
      </p>
    </div>
  </section>
</template>
