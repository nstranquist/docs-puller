import AxeBuilder from "@axe-core/playwright"
import { expect, test, type Page } from "@playwright/test"

const corpus = {
  id: "public-sample-v1",
  digest: `sha256:${"1".repeat(64)}`,
  index_digest: `sha256:${"2".repeat(64)}`,
  document_count: 24,
  source_count: 3,
  retrieved_at: "2026-08-19T01:06:48.000Z",
} as const

test.beforeEach(async ({ page }) => {
  await installDemoAPI(page)
})

test("searches, previews a document, and keeps the primary journey accessible", async ({
  page,
}, testInfo) => {
  await page.goto("/demo/")

  await expect(
    page.getByRole("heading", { name: "Ask the engine. Inspect the evidence." })
  ).toBeVisible()
  await expect(page.getByText("SQLite FTS5 Extension")).toBeVisible()
  await expect(page.getByText("1 result from the public sample")).toBeVisible()

  await page.keyboard.press("/")
  await expect(
    page.getByRole("textbox", { name: "What do you need to find?" })
  ).toBeFocused()

  await page.getByRole("button", { name: "Preview" }).click()
  const dialog = page.getByRole("dialog")
  await expect(dialog).toBeVisible()
  await expect(
    dialog.getByRole("heading", { name: "SQLite FTS5 Extension", exact: true })
  ).toBeVisible()
  await expect(dialog.locator("pre")).toContainText("External content tables")
  await expect(dialog.locator('[data-slot="dialog-footer"]')).toBeInViewport()
  await expect(
    dialog.getByRole("link", { name: "Open canonical source" })
  ).toBeInViewport()

  const overflow = await page.evaluate(
    () => document.documentElement.scrollWidth - window.innerWidth
  )
  expect(overflow).toBeLessThanOrEqual(1)

  const accessibility = await new AxeBuilder({ page })
    .exclude("[data-axe-skip]")
    .analyze()
  expect(
    accessibility.violations.filter(
      (violation) =>
        violation.impact === "critical" || violation.impact === "serious"
    )
  ).toEqual([])

  await page.screenshot({
    path: testInfo.outputPath("demo-journey.png"),
    fullPage: true,
  })
})

test("renders every public page without horizontal overflow", async ({
  page,
}) => {
  for (const path of ["/", "/demo/", "/method/"]) {
    await page.goto(path)
    await expect(page.locator("main")).toBeVisible()
    const dimensions = await page.evaluate(() => ({
      width: window.innerWidth,
      scrollWidth: document.documentElement.scrollWidth,
    }))
    expect(dimensions.scrollWidth).toBeLessThanOrEqual(dimensions.width + 1)
  }
})

test("stays inside browser performance and discovery budgets", async ({
  page,
}, testInfo) => {
  const measurements: Array<{
    path: string
    metrics: Record<string, number>
  }> = []
  await page.addInitScript(() => {
    const state = { cls: 0, lcpMs: 0, tbtMs: 0 }
    Object.defineProperty(window, "__docsPullerPerformance", {
      value: state,
      configurable: false,
      enumerable: false,
      writable: false,
    })

    if (PerformanceObserver.supportedEntryTypes.includes("layout-shift")) {
      new PerformanceObserver((list) => {
        for (const entry of list.getEntries()) {
          const shift = entry as PerformanceEntry & {
            hadRecentInput: boolean
            value: number
          }
          if (!shift.hadRecentInput) state.cls += shift.value
        }
      }).observe({ type: "layout-shift", buffered: true })
    }
    if (
      PerformanceObserver.supportedEntryTypes.includes(
        "largest-contentful-paint"
      )
    ) {
      new PerformanceObserver((list) => {
        const entries = list.getEntries()
        state.lcpMs = entries.at(-1)?.startTime ?? state.lcpMs
      }).observe({ type: "largest-contentful-paint", buffered: true })
    }
    if (PerformanceObserver.supportedEntryTypes.includes("longtask")) {
      new PerformanceObserver((list) => {
        for (const entry of list.getEntries()) {
          state.tbtMs += Math.max(0, entry.duration - 50)
        }
      }).observe({ type: "longtask", buffered: true })
    }
  })

  for (const path of ["/", "/demo/", "/method/"]) {
    await page.goto(path, { waitUntil: "networkidle" })
    await page.waitForTimeout(250)

    const metrics = await page.evaluate(() => {
      const state = (
        window as typeof window & {
          __docsPullerPerformance?: {
            cls: number
            lcpMs: number
            tbtMs: number
          }
        }
      ).__docsPullerPerformance ?? { cls: 0, lcpMs: 0, tbtMs: 0 }
      const navigation = performance.getEntriesByType("navigation")[0] as
        PerformanceNavigationTiming | undefined
      const resources = performance.getEntriesByType(
        "resource"
      ) as PerformanceResourceTiming[]
      const encodedBytes = (entry: PerformanceResourceTiming): number =>
        Math.max(entry.transferSize, entry.encodedBodySize)
      const fcp = performance.getEntriesByName("first-contentful-paint")[0]

      return {
        cls: state.cls,
        domContentLoadedMs: navigation?.domContentLoadedEventEnd ?? 0,
        fcpMs: fcp?.startTime ?? 0,
        lcpMs: state.lcpMs,
        loadMs: navigation?.loadEventEnd ?? 0,
        scriptBytes: resources
          .filter((entry) => entry.initiatorType === "script")
          .reduce((total, entry) => total + encodedBytes(entry), 0),
        styleBytes: resources
          .filter((entry) => new URL(entry.name).pathname.endsWith(".css"))
          .reduce((total, entry) => total + encodedBytes(entry), 0),
        tbtMs: state.tbtMs,
        transferBytes: resources.reduce(
          (total, entry) => total + encodedBytes(entry),
          0
        ),
      }
    })
    measurements.push({ path, metrics })

    expect(metrics.fcpMs, `${path} FCP`).toBeLessThanOrEqual(1_800)
    expect(metrics.lcpMs, `${path} LCP`).toBeGreaterThan(0)
    expect(metrics.lcpMs, `${path} LCP`).toBeLessThanOrEqual(2_500)
    expect(metrics.cls, `${path} CLS`).toBeLessThanOrEqual(0.1)
    expect(metrics.tbtMs, `${path} TBT`).toBeLessThanOrEqual(200)
    expect(
      metrics.domContentLoadedMs,
      `${path} DOMContentLoaded`
    ).toBeLessThanOrEqual(2_000)
    expect(metrics.loadMs, `${path} load`).toBeLessThanOrEqual(3_000)
    expect(metrics.scriptBytes, `${path} script bytes`).toBeLessThanOrEqual(
      250_000
    )
    expect(metrics.styleBytes, `${path} style bytes`).toBeLessThanOrEqual(
      250_000
    )
    expect(metrics.transferBytes, `${path} transfer bytes`).toBeLessThanOrEqual(
      750_000
    )

    await expect(page).toHaveTitle(/\S+/u)
    await expect(page.locator('html[lang="en"]')).toHaveCount(1)
    await expect(page.locator('meta[name="description"]')).toHaveAttribute(
      "content",
      /\S+/u
    )
    await expect(page.locator('link[rel="canonical"]')).toHaveAttribute(
      "href",
      /^https:\/\/docs-puller-demo\.darthbitcoin\.workers\.dev\//u
    )
  }

  await testInfo.attach("performance-budgets.json", {
    body: Buffer.from(JSON.stringify(measurements, null, 2)),
    contentType: "application/json",
  })
})

test("keeps the dark theme accessible", async ({ page }) => {
  await page.goto("/demo/")
  await page.getByRole("button", { name: "Use dark theme" }).click()
  await expect(
    page.getByRole("button", { name: "Use light theme" })
  ).toBeVisible()

  const accessibility = await new AxeBuilder({ page }).analyze()
  expect(
    accessibility.violations.filter(
      (violation) =>
        violation.impact === "critical" || violation.impact === "serious"
    )
  ).toEqual([])
})

async function installDemoAPI(page: Page): Promise<void> {
  await page.route("**/api/v1/demo/**", async (route) => {
    const requestURL = new URL(route.request().url())
    const headers = {
      "Content-Type": "application/json; charset=utf-8",
      "X-Request-ID": "playwright-synthetic",
    }
    switch (requestURL.pathname) {
      case "/api/v1/demo/meta":
        await route.fulfill({
          headers,
          json: {
            ok: true,
            schema_version: 1,
            service: "docs-puller-demo",
            engine: { name: "docs-puller", version: "v0.6.0", mode: "fts5" },
            build_id: "e2e-build",
            commit: "abcdef0123456789",
            deployed_at: "2026-08-19T02:00:00.000Z",
            corpus,
            limits: {
              query_characters: 160,
              results: 10,
              timeout_ms: 4000,
              response_bytes: 65536,
            },
          },
        })
        return
      case "/api/v1/demo/sources":
        await route.fulfill({
          headers,
          json: {
            ok: true,
            corpus,
            sources: [
              {
                id: "sqlite",
                label: "SQLite",
                document_count: 8,
                homepage: "https://sqlite.org/docs.html",
                license: "Public domain documentation",
              },
              {
                id: "go",
                label: "Go",
                document_count: 8,
                homepage: "https://go.dev/doc/",
                license: "CC BY 4.0 website content, unless noted otherwise",
              },
              {
                id: "postgresql",
                label: "PostgreSQL",
                document_count: 8,
                homepage: "https://www.postgresql.org/docs/",
                license: "PostgreSQL documentation license",
              },
            ],
          },
        })
        return
      case "/api/v1/demo/search":
        await route.fulfill({
          headers,
          json: {
            ok: true,
            query:
              requestURL.searchParams.get("q") ??
              "fts5 external content tables",
            engine: "docs-puller",
            mode: "fts5",
            elapsed_ms: 3,
            result_count: 1,
            corpus,
            results: [
              {
                title: "SQLite FTS5 Extension",
                source: "sqlite",
                path: "fts5.md",
                url: "https://sqlite.org/fts5.html",
                score: 944,
                snippets: [
                  {
                    line: 513,
                    text: "External content and contentless tables.",
                  },
                ],
              },
            ],
          },
        })
        return
      case "/api/v1/demo/doc":
        await route.fulfill({
          headers,
          json: {
            ok: true,
            source: "sqlite",
            path: "fts5.md",
            title: "SQLite FTS5 Extension",
            url: "https://sqlite.org/fts5.html",
            content_type: "text/markdown",
            content:
              "# SQLite FTS5 Extension\n\n## External content tables\n\nThe index refers to content in another table.",
            bytes: 100,
            total_bytes: 165924,
            truncated: true,
            start_line: 500,
            end_line: 525,
            total_lines: 3100,
            corpus,
          },
        })
        return
      default:
        await route.fulfill({ status: 404, headers, json: { ok: false } })
    }
  })
}
