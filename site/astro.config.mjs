// @ts-check

import react from "@astrojs/react"
import tailwindcss from "@tailwindcss/vite"
import { defineConfig } from "astro/config"

export default defineConfig({
  site: "https://docs-puller-demo.darthbitcoin.workers.dev",
  output: "static",
  trailingSlash: "always",
  build: {
    assets: "assets",
    format: "directory",
    inlineStylesheets: "auto",
  },
  integrations: [react()],
  vite: {
    plugins: [tailwindcss()],
    build: {
      target: "es2022",
    },
  },
})
