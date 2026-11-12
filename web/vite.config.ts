import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'
import { fileURLToPath, URL } from 'node:url'
import { writeFileSync } from 'node:fs'

export default defineConfig({
  base: '/portal/',
  plugins: [
    react(),
    tailwindcss(),
    {
      name: 'embed-placeholder',
      closeBundle() {
        writeFileSync('dist/.keep', '')
      },
    },
  ],
  resolve: { alias: { '@': fileURLToPath(new URL('./src', import.meta.url)) } },
  build: { emptyOutDir: true },
  server: {
    proxy: Object.fromEntries(
      [
        '/portal/api',
        '/portal/login',
        '/portal/sign-in/keycloak',
        '/portal/password',
        '/portal/callback',
        '/portal/logout',
      ].map((path) => [
        path,
        {
          target: process.env.OLR_DEV_ROUTER ?? 'http://localhost:8080',
          changeOrigin: true,
        },
      ]),
    ),
  },
})
