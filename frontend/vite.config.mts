import path from 'node:path'
import {defineConfig} from 'vite'
import vue from '@vitejs/plugin-vue'
export default defineConfig({plugins:[vue()],build:{outDir:path.resolve(import.meta.dirname,'../.frontend-check/bundle'),emptyOutDir:true,minify:false,lib:{entry:path.resolve(import.meta.dirname,'index.ts'),formats:['es'],fileName:'fallback-html'},rollupOptions:{external:['@solovey/component-ui','@solovey/component-ui/api','vue','vue-i18n']}}})
