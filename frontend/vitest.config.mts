import path from 'node:path'
import {defineConfig} from 'vitest/config'
import vue from '@vitejs/plugin-vue'
const sdk=process.env.SOLOVEY_COMPONENT_UI_SDK
if(!sdk)throw new Error('accepted Component UI SDK source is required')
export default defineConfig({plugins:[vue()],resolve:{alias:[{find:/^@solovey\/component-ui\/api$/,replacement:path.join(sdk,'src/componentSystem/api.ts')},{find:/^@solovey\/component-ui$/,replacement:path.join(sdk,'src/componentSystem/sdk.ts')},{find:'@',replacement:path.join(sdk,'src')},{find:/^vue$/,replacement:path.join(sdk,'node_modules/vue/dist/vue.runtime.esm-bundler.js')}],dedupe:['vue','vue-i18n','vue-router','pinia']},test:{environment:'node',include:['**/*.test.ts'],css:false,testTimeout:30000,server:{deps:{inline:['vuetify']}}}})
