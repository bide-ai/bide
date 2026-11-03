import { ref } from 'vue'

// Roving-tabindex tab behaviour shared by the homepage tab groups: click selects, Left/Right move
// and wrap, Home/End jump. Focus follows selection.
export function useTabs(ids: string[], prefix: string) {
  const active = ref(ids[0])

  function select(id: string, focus = false) {
    active.value = id
    if (focus && typeof document !== 'undefined') {
      document.getElementById(`${prefix}-tab-${id}`)?.focus()
    }
  }

  function onKey(e: KeyboardEvent, i: number) {
    let next = -1
    if (e.key === 'ArrowRight') next = (i + 1) % ids.length
    else if (e.key === 'ArrowLeft') next = (i - 1 + ids.length) % ids.length
    else if (e.key === 'Home') next = 0
    else if (e.key === 'End') next = ids.length - 1
    if (next < 0) return
    e.preventDefault()
    select(ids[next], true)
  }

  return { active, select, onKey }
}
