import { describe, expect, it } from "vitest"

import { GET as robotsGET } from "@/pages/robots.txt"
import { GET as sitemapGET } from "@/pages/sitemap.xml"

describe("static discovery routes", () => {
  it("publishes a robots file with the canonical sitemap", async () => {
    const context = {
      site: new URL("https://docs-puller-demo.example/"),
    } as Parameters<typeof robotsGET>[0]
    const response = await robotsGET(context)
    expect(response).toBeInstanceOf(Response)
    const resolved = response as Response
    expect(resolved.headers.get("Content-Type")).toContain("text/plain")
    expect(await resolved.text()).toBe(
      "User-agent: *\nAllow: /\nSitemap: https://docs-puller-demo.example/sitemap.xml\n"
    )
  })

  it("publishes only the three canonical product routes", async () => {
    const context = {
      site: new URL("https://docs-puller-demo.example/"),
    } as Parameters<typeof sitemapGET>[0]
    const response = (await sitemapGET(context)) as Response
    const xml = await response.text()
    expect(response.headers.get("Content-Type")).toContain("application/xml")
    expect(xml).toContain("<loc>https://docs-puller-demo.example/</loc>")
    expect(xml).toContain("<loc>https://docs-puller-demo.example/demo/</loc>")
    expect(xml).toContain("<loc>https://docs-puller-demo.example/method/</loc>")
    expect(xml.match(/<url>/gu)).toHaveLength(3)
  })
})
