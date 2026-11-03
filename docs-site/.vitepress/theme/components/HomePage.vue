<script setup lang="ts">
import HomeApi from './HomeApi.vue'
import HomeApprovals from './HomeApprovals.vue'
import HomeBlog from './HomeBlog.vue'
import HomeCompare from './HomeCompare.vue'
import HomeFooter from './HomeFooter.vue'
import HomeFramework from './HomeFramework.vue'
import HomeGuarantees from './HomeGuarantees.vue'
import HomeHero from './HomeHero.vue'
import HomeLadder from './HomeLadder.vue'
import HomeProblem from './HomeProblem.vue'
import HomeProof from './HomeProof.vue'
import HomeStart from './HomeStart.vue'
import HomeTools from './HomeTools.vue'
import HomeUntrusted from './HomeUntrusted.vue'
</script>

<template>
  <div class="bh">
    <main id="home-main">
    <HomeHero />
    <HomeProblem />
    <HomeLadder />
    <HomeApi />
    <HomeFramework />
    <HomeGuarantees />
    <HomeApprovals />
    <HomeUntrusted />
    <HomeTools />
    <HomeCompare />
    <HomeProof />
    <HomeStart />
    <HomeBlog />
    </main>
    <HomeFooter />
  </div>
</template>
