import { readFileSync } from 'node:fs'
import tailwindcss from '@tailwindcss/vite'
import { defineConfig, type ProxyOptions } from 'vite'

// Production is same-origin behind the OIDC edge. Only this dev proxy handles
// credentials; browser code never sees a bearer or a cookie-file's contents.
const upstream = process.env.KNOWLEDGE_API_UPSTREAM ?? `http://127.0.0.1:${process.env.KNOWLEDGE_MOCK_PORT ?? 5206}`
const devBearer = process.env.KNOWLEDGE_DEV_BEARER
const devCookieFile = process.env.KNOWLEDGE_DEV_COOKIE_FILE
const proxy: ProxyOptions = {
  target: upstream,
  changeOrigin: true,
  configure(server) {
    server.on('proxyReq', (request) => {
      request.removeHeader('origin')
      if (devBearer) request.setHeader('authorization', `Bearer ${devBearer}`)
      if (devCookieFile) request.setHeader('cookie', readFileSync(devCookieFile, 'utf8').trim())
    })
  },
}

export default defineConfig({
  plugins: [tailwindcss()],
  esbuild: { jsx: 'automatic' },
  server: {
    host: '127.0.0.1',
    port: Number(process.env.KNOWLEDGE_PORT ?? 5208),
    strictPort: true,
    proxy: { '/api': proxy, '/config': proxy, '/releasez': proxy, '/launcher': proxy },
  },
})
