import type { APIRoute } from "astro"

const routes = ["/", "/demo/", "/method/"]

export const GET: APIRoute = ({ site }) => {
  const urls = routes
    .map(
      (route) =>
        `<url><loc>${escapeXML(new URL(route, site).toString())}</loc></url>`
    )
    .join("")
  return new Response(
    `<?xml version="1.0" encoding="UTF-8"?><urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">${urls}</urlset>`,
    { headers: { "Content-Type": "application/xml; charset=utf-8" } }
  )
}

function escapeXML(value: string): string {
  return value
    .replaceAll("&", "&amp;")
    .replaceAll("<", "&lt;")
    .replaceAll(">", "&gt;")
}
