import { withBase } from 'vitepress'

// Homepage images are copied into public/home/ by sync-docs.mjs.
export const homeImg = (file: string) => withBase(`/home/${file}`)
