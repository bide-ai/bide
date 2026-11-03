<script setup lang="ts">
import { data } from '../version.data'
import { audiences, hero } from '../home-data'
import { homeImg } from '../img'
</script>

<template>
  <section class="h-hero">
    <div class="wrap h-hero-grid">
      <div class="h-hero-copy">
        <p class="h-badge mono">
          <span class="k">v{{ data.version }}</span><span class="p">·</span>Apache-2.0<span class="p">·</span>Go {{ data.go }}<span class="p">·</span>pre-1.0
        </p>
        <h1 class="h-h1">{{ hero.title }}</h1>
        <p class="lede h-hero-lede">{{ hero.lede }}</p>
        <div class="term h-install">
          <pre class="code" v-html="hero.installHtml"></pre>
        </div>
        <div class="h-actions">
          <a class="btn btn-p" href="/getting-started">Get started</a>
          <a class="btn btn-s" href="https://github.com/bide-ai/bide">View on GitHub</a>
        </div>
        <p class="small h-hero-note">
          {{ hero.note }} <span class="mono h-cmd-inline">{{ hero.noteCmd }}</span>
        </p>
      </div>
      <div class="h-hero-fig">
        <figure>
          <div class="term h-glow">
            <div class="term-bar"><span class="dot"></span><span class="dot"></span><span class="dot"></span><span class="mono h-bar-title">bide demo</span></div>
            <img
              :src="homeImg(hero.figure.src)"
              :alt="hero.figure.alt"
              :width="hero.figure.width"
              :height="hero.figure.height"
              loading="lazy"
              decoding="async"
              class="h-hero-img"
            />
          </div>
          <figcaption class="small">{{ hero.caption }}</figcaption>
        </figure>
      </div>
    </div>
  </section>

  <section class="h-audience">
    <div class="wrap grid g3">
      <div v-for="a in audiences" :key="a.title" class="card">
        <p class="mono k h-card-label">{{ a.title }}</p>
        <p class="muted h-flush">{{ a.text }}</p>
      </div>
    </div>
  </section>
</template>
