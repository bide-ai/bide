import { h } from 'vue'
import type { Theme } from 'vitepress'
import { useRoute } from 'vitepress'
import DefaultTheme from 'vitepress/theme'
import HomePage from './components/HomePage.vue'
import './custom.css'
import './home.css'

// The homepage is dark-only. VitePress's dark palette is defined on the `.dark` class, which works
// on any element, so on the homepage route the Layout root gets `.dark`. The nav bar and the page
// inside it then use the dark palette whatever the appearance toggle says. Nothing on <html> is
// touched, so the stored preference is never changed and docs pages keep the light/dark toggle.
// The class is part of the server-rendered HTML for "/", so there is no flash.
const isHome = (path: string) => path === '/' || path === '/index.html' || path === '/index'

export default {
  extends: DefaultTheme,
  Layout() {
    const route = useRoute()
    return h(DefaultTheme.Layout, { class: isHome(route.path) ? 'dark' : undefined })
  },
  enhanceApp({ app }) {
    app.component('HomePage', HomePage)
  },
} satisfies Theme
