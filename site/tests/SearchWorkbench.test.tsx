import { axe } from "vitest-axe"
import { fireEvent, render, screen, waitFor } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { beforeEach, describe, expect, it, vi } from "vitest"

import { SearchWorkbench } from "@/components/SearchWorkbench"
import { DemoAPIError } from "@/lib/api/client"
import {
  createFixtureClient,
  documentFixture,
  metaFixture,
  searchFixture,
  sourcesFixture,
} from "./fixtures"

describe("SearchWorkbench", () => {
  beforeEach(() => {
    history.replaceState(null, "", "/demo/")
  })

  it("renders a successful search with cited, highlighted evidence", async () => {
    const search = vi.fn(async () => searchFixture)
    const client = createFixtureClient({ search })
    const user = userEvent.setup()
    render(<SearchWorkbench client={client} autoSearch={false} />)

    await user.click(screen.getByRole("button", { name: "Search" }))

    expect(await screen.findByText("SQLite FTS5 Extension")).toBeInTheDocument()
    expect(
      screen.getByText("1 result from the public sample")
    ).toBeInTheDocument()
    expect(screen.getByText("2 ms engine")).toBeInTheDocument()
    expect(
      screen.getByText("external", { selector: "mark" })
    ).toBeInTheDocument()
    expect(search).toHaveBeenCalledWith(
      { q: "fts5 external content tables", limit: 6 },
      expect.any(AbortSignal)
    )
    expect(window.location.search).toContain("q=fts5+external+content+tables")
  })

  it("loads metadata and source options without blocking search", async () => {
    const meta = vi.fn(async () => metaFixture)
    const sources = vi.fn(async () => sourcesFixture)
    render(
      <SearchWorkbench
        client={createFixtureClient({ meta, sources })}
        autoSearch={false}
      />
    )
    expect(await screen.findByText("Canonical engine")).toBeInTheDocument()
    await waitFor(() => {
      expect(meta).toHaveBeenCalledOnce()
      expect(sources).toHaveBeenCalledOnce()
    })
    expect(
      screen.getByRole("option", { name: "PostgreSQL" })
    ).toBeInTheDocument()
    expect(screen.getByText(/docs-puller v0\.6\.0/)).toBeInTheDocument()
  })

  it("restores a bounded shareable query and source from the URL", async () => {
    history.replaceState(
      null,
      "",
      "/demo/?q=fan-in%20fan-out%20channel%20pipelines&source=go"
    )
    const search = vi.fn(async () => ({
      ...searchFixture,
      query: "fan-in fan-out channel pipelines",
    }))
    render(<SearchWorkbench client={createFixtureClient({ search })} />)
    await waitFor(() =>
      expect(search).toHaveBeenCalledWith(
        { q: "fan-in fan-out channel pipelines", source: "go", limit: 6 },
        expect.any(AbortSignal)
      )
    )
    expect(
      screen.getByRole("textbox", { name: "What do you need to find?" })
    ).toHaveValue("fan-in fan-out channel pipelines")
    expect(
      screen.getByRole("combobox", { name: "Filter by source" })
    ).toHaveValue("go")
  })

  it("runs a suggested query with its closed source", async () => {
    const search = vi.fn(async () => ({
      ...searchFixture,
      query: "fan-in fan-out channel pipelines",
      results: [{ ...searchFixture.results[0]!, source: "go" as const }],
    }))
    const user = userEvent.setup()
    render(
      <SearchWorkbench
        client={createFixtureClient({ search })}
        autoSearch={false}
      />
    )
    await user.click(screen.getByRole("button", { name: "Go pipelines" }))
    await waitFor(() =>
      expect(search).toHaveBeenCalledWith(
        { q: "fan-in fan-out channel pipelines", source: "go", limit: 6 },
        expect.any(AbortSignal)
      )
    )
    expect(
      screen.getByRole("combobox", { name: "Filter by source" })
    ).toHaveValue("go")
  })

  it("validates the visible-character limit before sending a request", async () => {
    const search = vi.fn(async () => searchFixture)
    const user = userEvent.setup()
    render(
      <SearchWorkbench
        client={createFixtureClient({ search })}
        autoSearch={false}
      />
    )
    const input = screen.getByRole("textbox", {
      name: "What do you need to find?",
    })
    await user.clear(input)
    await user.type(input, "x")
    await user.click(screen.getByRole("button", { name: "Search" }))
    expect(screen.getByRole("alert")).toHaveTextContent("2 to 160")
    expect(search).not.toHaveBeenCalled()
    expect(input).toHaveFocus()
  })

  it("supports the slash focus shortcut and Escape blur", async () => {
    render(
      <SearchWorkbench client={createFixtureClient()} autoSearch={false} />
    )
    const input = screen.getByRole("textbox", {
      name: "What do you need to find?",
    })
    fireEvent.keyDown(window, { key: "/" })
    expect(input).toHaveFocus()
    fireEvent.keyDown(input, { key: "Escape" })
    expect(input).not.toHaveFocus()
  })

  it("explains rate limits without leaking implementation errors", async () => {
    const search = vi.fn(async () => {
      throw new DemoAPIError(429, {
        ok: false,
        error: {
          code: "rate_limited",
          message: "budget",
          request_id: "request-1",
          retry_after_seconds: 60,
        },
      })
    })
    const user = userEvent.setup()
    render(
      <SearchWorkbench
        client={createFixtureClient({ search })}
        autoSearch={false}
      />
    )
    await user.click(screen.getByRole("button", { name: "Search" }))
    expect(
      await screen.findByText(/Try again in 60 seconds/)
    ).toBeInTheDocument()
    expect(screen.queryByText("budget")).not.toBeInTheDocument()
  })

  it("shows safe empty and network-failure states", async () => {
    const emptySearch = vi.fn(async () => ({
      ...searchFixture,
      result_count: 0,
      results: [],
    }))
    const user = userEvent.setup()
    const view = render(
      <SearchWorkbench
        client={createFixtureClient({ search: emptySearch })}
        autoSearch={false}
      />
    )
    await user.click(screen.getByRole("button", { name: "Search" }))
    expect(await screen.findByText("No exact result")).toBeInTheDocument()

    view.unmount()
    render(
      <SearchWorkbench
        client={createFixtureClient({
          search: async () => {
            throw new Error("private transport detail")
          },
        })}
        autoSearch={false}
      />
    )
    await user.click(screen.getByRole("button", { name: "Search" }))
    expect(
      await screen.findByText(/network request failed/i)
    ).toBeInTheDocument()
    expect(
      screen.queryByText("private transport detail")
    ).not.toBeInTheDocument()
  })

  it("copies a reproducible CLI command", async () => {
    const writeText = vi.fn(async () => undefined)
    const user = userEvent.setup()
    Object.defineProperty(navigator, "clipboard", {
      configurable: true,
      value: { writeText },
    })
    render(
      <SearchWorkbench client={createFixtureClient()} autoSearch={false} />
    )
    await user.click(screen.getByRole("button", { name: "Copy CLI command" }))
    expect(writeText).toHaveBeenCalledWith(
      'docs-puller search "fts5 external content tables" --json'
    )
    expect(screen.getByRole("button", { name: "Copied" })).toBeInTheDocument()
  })

  it("previews Markdown as text and links to the canonical source", async () => {
    const document = vi.fn(async () => documentFixture)
    const user = userEvent.setup()
    render(
      <SearchWorkbench
        client={createFixtureClient({ document })}
        autoSearch={false}
      />
    )
    await user.click(screen.getByRole("button", { name: "Search" }))
    await user.click(await screen.findByRole("button", { name: "Preview" }))
    expect(
      await screen.findByText(/External content tables use/)
    ).toBeInTheDocument()
    expect(document).toHaveBeenCalledWith(
      { source: "sqlite", path: "fts5.md", line: 911 },
      expect.any(AbortSignal)
    )
    expect(
      screen.getByRole("link", { name: /Open canonical source/ })
    ).toHaveAttribute("href", "https://sqlite.org/fts5.html")
    expect(screen.getByText(/lines 900–925 of 3,100/)).toBeInTheDocument()
  })

  it("has no automated accessibility violations in the idle workbench", async () => {
    const { container } = render(
      <main>
        <h1>Live demo</h1>
        <SearchWorkbench client={createFixtureClient()} autoSearch={false} />
      </main>
    )
    await screen.findByText("Canonical engine")
    expect(
      (
        await axe(container, {
          rules: { "color-contrast": { enabled: false } },
        })
      ).violations
    ).toEqual([])
  })
})
