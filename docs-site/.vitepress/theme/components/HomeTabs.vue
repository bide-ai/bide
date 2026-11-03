<script setup lang="ts">
// Accessible tab group. Each tab's panel content comes from the slot named by the tab id.
import { useTabs } from '../useTabs'

const props = defineProps<{
  tabs: { id: string; label: string }[]
  label: string
  /** Prefix for element ids; unique per group. */
  name: string
  /** 'term' wraps the group in a terminal-style box. */
  variant?: 'term' | 'plain'
}>()

const { active, select, onKey } = useTabs(
  props.tabs.map((t) => t.id),
  props.name,
)
</script>

<template>
  <div class="h-tabs" :class="variant === 'term' ? 'term' : ''">
    <div class="h-tablist" :class="variant === 'term' ? 'term-bar' : ''" role="tablist" :aria-label="label">
      <button
        v-for="(t, i) in tabs"
        :id="`${name}-tab-${t.id}`"
        :key="t.id"
        class="tab"
        type="button"
        role="tab"
        :aria-selected="active === t.id"
        :aria-controls="`${name}-panel-${t.id}`"
        :tabindex="active === t.id ? 0 : -1"
        @click="select(t.id)"
        @keydown="onKey($event, i)"
      >
        {{ t.label }}
      </button>
    </div>
    <div
      v-for="t in tabs"
      :id="`${name}-panel-${t.id}`"
      :key="t.id"
      class="h-tabpanel"
      role="tabpanel"
      :aria-labelledby="`${name}-tab-${t.id}`"
      :hidden="active !== t.id"
      tabindex="0"
    >
      <slot :name="t.id" />
    </div>
  </div>
</template>
