import { fileURLToPath, URL } from 'node:url'
import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

// 构建产物直接落到 internal/web/dist，由 internal/web/embed.go 的
// //go:embed all:dist 编入 sinyefrp.exe。因此 outDir 必须与 embed 路径一致，
// 且 emptyOutDir 必须为 true —— 否则上一版残留的哈希资源会一起被打进 exe。
export default defineConfig({
  plugins: [vue()],
  resolve: {
    alias: {
      '@': fileURLToPath(new URL('./src', import.meta.url)),
    },
  },
  build: {
    outDir: '../internal/web/dist',
    emptyOutDir: true,
    // 目标浏览器只有 Edge（Chromium），且按 02-技术选型 §6.1 不引 IE 垫片。
    target: 'chrome100',
    // 单文件 exe 的静态资源走 go:embed，拆成大量小文件没有收益，
    // 但也不能太大导致首屏等待；这里保留 Vite 默认的 500KB 提示线。
    chunkSizeWarningLimit: 800,
    rollupOptions: {
      output: {
        // 把体积最大的 Element Plus 单独成块，便于浏览器缓存与排障。
        manualChunks: {
          vendor: ['vue', 'vue-router', 'pinia'],
          ui: ['element-plus', '@element-plus/icons-vue'],
        },
      },
    },
  },
  server: {
    host: '127.0.0.1',
    port: 5173,
    // 开发期把 /api 代理到 Go 服务，避免开发时需要处理跨域与 Cookie 域问题。
    // 生产是同一个进程同源提供，不存在这层代理。
    proxy: {
      '/api': {
        target: 'http://127.0.0.1:7800',
        changeOrigin: false,
      },
    },
  },
})
