import { createApp } from 'vue'
import { createPinia } from 'pinia'
import ElementPlus from 'element-plus'
import zhCn from 'element-plus/es/locale/lang/zh-cn'
import * as ElementPlusIconsVue from '@element-plus/icons-vue'
import 'element-plus/dist/index.css'

import App from './App.vue'
import { router } from './router'
import { useSessionStore } from './stores/session'
import './styles/index.css'

const app = createApp(App)
const pinia = createPinia()

app.use(pinia)
app.use(router)
app.use(ElementPlus, { locale: zhCn, size: 'default' })

// 图标按需注册为全局组件：Element Plus 的图标集是独立包，
// 逐个 import 会让每个视图顶部堆一大串 import，收益不抵噪声。
for (const [name, component] of Object.entries(ElementPlusIconsVue)) {
  app.component(name, component)
}

// 必须在挂载前装好 401 拦截：会话过期时它负责把界面退回登录页。
useSessionStore().installInterceptor()

app.mount('#app')
