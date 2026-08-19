import {
  ArrowUpRight,
  Check,
  Clipboard,
  Clock3,
  Code2,
  FileText,
  RotateCcw,
  Search,
  ShieldCheck,
  Sparkles,
} from "lucide-react"
import {
  type ComponentProps,
  type KeyboardEvent as ReactKeyboardEvent,
  type ReactNode,
  useCallback,
  useEffect,
  useMemo,
  useRef,
  useState,
} from "react"

import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardFooter, CardHeader } from "@/components/ui/card"
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog"
import { Input } from "@/components/ui/input"
import { Skeleton } from "@/components/ui/skeleton"
import {
  createDemoClient,
  DemoAPIError,
  normalizeQuery,
  type DemoClient,
  type DocumentResponse,
  type MetaResponse,
  type SearchResponse,
  type SearchResult,
  type SourceID,
  type SourcesResponse,
} from "@/lib/api/client"

const suggestedQueries = [
  {
    label: "SQLite FTS5",
    query: "fts5 external content tables",
    source: "sqlite",
  },
  {
    label: "Go pipelines",
    query: "fan-in fan-out channel pipelines",
    source: "go",
  },
  {
    label: "Postgres indexes",
    query: "create index concurrently locking",
    source: "postgresql",
  },
  {
    label: "Natural language",
    query: "How do slices grow when appending?",
    source: "go",
  },
] as const satisfies ReadonlyArray<{
  label: string
  query: string
  source: SourceID
}>

const sourceLabels: Record<SourceID, string> = {
  sqlite: "SQLite",
  go: "Go",
  postgresql: "PostgreSQL",
}

const defaultClient = createDemoClient()

type RequestState =
  | { kind: "idle" }
  | { kind: "loading" }
  | { kind: "success"; data: SearchResponse }
  | { kind: "error"; error: Error }

type DocumentState =
  | { kind: "idle" }
  | { kind: "loading" }
  | { kind: "success"; data: DocumentResponse }
  | { kind: "error"; error: Error }

interface SearchWorkbenchProps {
  client?: DemoClient
  autoSearch?: boolean
}

export function SearchWorkbench({
  client = defaultClient,
  autoSearch = true,
}: SearchWorkbenchProps) {
  const inputRef = useRef<HTMLInputElement>(null)
  const searchAbortRef = useRef<AbortController | null>(null)
  const documentAbortRef = useRef<AbortController | null>(null)
  const copyTimerRef = useRef<number | undefined>(undefined)
  const [query, setQuery] = useState<string>(suggestedQueries[0].query)
  const [source, setSource] = useState<SourceID | "">("")
  const [request, setRequest] = useState<RequestState>({ kind: "idle" })
  const [meta, setMeta] = useState<MetaResponse | null>(null)
  const [sources, setSources] = useState<SourcesResponse | null>(null)
  const [selectedResult, setSelectedResult] = useState<SearchResult | null>(
    null
  )
  const [document, setDocument] = useState<DocumentState>({ kind: "idle" })
  const [copied, setCopied] = useState(false)
  const [validationMessage, setValidationMessage] = useState("")

  const executeSearch = useCallback(
    async (nextQuery: string, nextSource: SourceID | "") => {
      const normalized = normalizeQuery(nextQuery)
      if (normalized.length < 2 || normalized.length > 160) {
        setValidationMessage("Enter a query with 2 to 160 characters.")
        inputRef.current?.focus()
        return
      }

      setValidationMessage("")
      setQuery(normalized)
      setSource(nextSource)
      searchAbortRef.current?.abort()
      const controller = new AbortController()
      searchAbortRef.current = controller
      setRequest({ kind: "loading" })

      try {
        const data = await client.search(
          {
            q: normalized,
            ...(nextSource ? { source: nextSource } : {}),
            limit: 6,
          },
          controller.signal
        )
        setRequest({ kind: "success", data })
        if (typeof window !== "undefined") {
          const url = new URL(window.location.href)
          url.searchParams.set("q", normalized)
          if (nextSource) url.searchParams.set("source", nextSource)
          else url.searchParams.delete("source")
          history.replaceState(null, "", url)
        }
      } catch (error) {
        if (controller.signal.aborted) return
        setRequest({ kind: "error", error: asError(error) })
      }
    },
    [client]
  )

  useEffect(() => {
    const controller = new AbortController()
    void Promise.allSettled([
      client.meta(controller.signal),
      client.sources(controller.signal),
    ]).then(([metaResult, sourcesResult]) => {
      if (controller.signal.aborted) return
      if (metaResult.status === "fulfilled") setMeta(metaResult.value)
      if (sourcesResult.status === "fulfilled") setSources(sourcesResult.value)
    })

    if (autoSearch) {
      let initialQuery: string = suggestedQueries[0].query
      let initialSource: SourceID | "" = ""
      if (typeof window !== "undefined") {
        const params = new URLSearchParams(window.location.search)
        const fromURL = normalizeQuery(params.get("q") ?? "")
        if (fromURL.length >= 2 && fromURL.length <= 160) initialQuery = fromURL
        initialSource = parseSource(params.get("source"))
      }
      queueMicrotask(() => {
        if (!controller.signal.aborted)
          void executeSearch(initialQuery, initialSource)
      })
    }

    return () => {
      controller.abort()
      searchAbortRef.current?.abort()
      documentAbortRef.current?.abort()
      if (copyTimerRef.current !== undefined)
        window.clearTimeout(copyTimerRef.current)
    }
  }, [autoSearch, client, executeSearch])

  useEffect(() => {
    const onShortcut = (event: globalThis.KeyboardEvent) => {
      const target = event.target
      const editable =
        target instanceof HTMLInputElement ||
        target instanceof HTMLTextAreaElement ||
        (target instanceof HTMLElement && target.isContentEditable)
      if (
        event.key === "/" &&
        !editable &&
        !event.metaKey &&
        !event.ctrlKey &&
        !event.altKey
      ) {
        event.preventDefault()
        inputRef.current?.focus()
      }
    }
    window.addEventListener("keydown", onShortcut)
    return () => window.removeEventListener("keydown", onShortcut)
  }, [])

  const handleSubmit: NonNullable<ComponentProps<"form">["onSubmit"]> = (
    event
  ) => {
    event.preventDefault()
    void executeSearch(query, source)
  }

  const chooseSuggestion = (suggestion: (typeof suggestedQueries)[number]) => {
    setQuery(suggestion.query)
    setSource(suggestion.source)
    void executeSearch(suggestion.query, suggestion.source)
  }

  const openDocument = async (result: SearchResult) => {
    setSelectedResult(result)
    setDocument({ kind: "loading" })
    documentAbortRef.current?.abort()
    const controller = new AbortController()
    documentAbortRef.current = controller
    try {
      const data = await client.document(
        { source: result.source, path: result.path },
        controller.signal
      )
      setDocument({ kind: "success", data })
    } catch (error) {
      if (controller.signal.aborted) return
      setDocument({ kind: "error", error: asError(error) })
    }
  }

  const copyCommand = async () => {
    const command = `docs-puller search ${JSON.stringify(normalizeQuery(query))} --json${source ? ` --source ${source}` : ""}`
    try {
      await navigator.clipboard.writeText(command)
      setCopied(true)
      if (copyTimerRef.current !== undefined)
        window.clearTimeout(copyTimerRef.current)
      copyTimerRef.current = window.setTimeout(() => setCopied(false), 1800)
    } catch {
      setCopied(false)
    }
  }

  const resultData = request.kind === "success" ? request.data : null
  const liveStatus =
    request.kind === "error" ? "degraded" : meta ? "live" : "checking"
  const sourceOptions = sources?.sources ?? []
  const command = useMemo(
    () =>
      `docs-puller search ${JSON.stringify(normalizeQuery(query))} --json${source ? ` --source ${source}` : ""}`,
    [query, source]
  )

  return (
    <div className="grid gap-6 xl:grid-cols-[minmax(0,1fr)_17rem]">
      <Card className="bg-card/92 min-w-0">
        <CardHeader className="border-border/70 border-b p-5 sm:p-6">
          <div className="flex flex-col gap-4 sm:flex-row sm:items-center sm:justify-between">
            <div className="flex items-center gap-3">
              <span className="bg-primary/10 text-primary flex size-10 items-center justify-center rounded-xl">
                <Search className="size-5" aria-hidden="true" />
              </span>
              <div>
                <h2 className="font-heading text-base font-semibold tracking-[-0.025em]">
                  Public search workbench
                </h2>
                <p className="text-muted-foreground mt-0.5 text-xs">
                  Real Go engine · closed public corpus
                </p>
              </div>
            </div>
            <Badge
              variant={
                liveStatus === "live"
                  ? "signal"
                  : liveStatus === "degraded"
                    ? "destructive"
                    : "outline"
              }
            >
              <span className="relative flex size-1.5">
                {liveStatus === "live" && (
                  <span className="bg-primary absolute inline-flex size-full animate-ping rounded-full opacity-40" />
                )}
                <span className="relative inline-flex size-1.5 rounded-full bg-current" />
              </span>
              {liveStatus}
            </Badge>
          </div>
        </CardHeader>

        <CardContent className="p-5 sm:p-6">
          <form
            onSubmit={handleSubmit}
            aria-label="Search the public docs corpus"
            noValidate
          >
            <label
              htmlFor="demo-query"
              className="mb-2 block text-sm font-semibold"
            >
              What do you need to find?
            </label>
            <div className="grid gap-2.5 sm:grid-cols-[minmax(0,1fr)_9.5rem_auto]">
              <div className="relative min-w-0">
                <Input
                  ref={inputRef}
                  id="demo-query"
                  value={query}
                  onChange={(event) => setQuery(event.target.value)}
                  onKeyDown={(event: ReactKeyboardEvent<HTMLInputElement>) => {
                    if (event.key === "Escape") event.currentTarget.blur()
                  }}
                  minLength={2}
                  maxLength={160}
                  autoComplete="off"
                  spellCheck="false"
                  aria-describedby="query-help query-error"
                  aria-invalid={Boolean(validationMessage)}
                  placeholder="Try: fts5 external content tables"
                  className="pr-12 font-mono text-[0.82rem]"
                />
                <span className="pointer-events-none absolute top-1/2 right-3 -translate-y-1/2">
                  <span className="kbd hidden sm:inline-flex">/</span>
                </span>
              </div>
              <label className="sr-only" htmlFor="demo-source">
                Filter by source
              </label>
              <select
                id="demo-source"
                value={source}
                onChange={(event) => setSource(parseSource(event.target.value))}
                className="border-input bg-background/88 focus-visible:border-primary/55 focus-visible:ring-primary/10 h-12 rounded-xl border px-3 text-sm font-medium transition-shadow outline-none focus-visible:ring-4"
              >
                <option value="">All sources</option>
                {sourceOptions.length > 0
                  ? sourceOptions.map((item) => (
                      <option key={item.id} value={item.id}>
                        {item.label}
                      </option>
                    ))
                  : Object.entries(sourceLabels).map(([id, label]) => (
                      <option key={id} value={id}>
                        {label}
                      </option>
                    ))}
              </select>
              <Button
                type="submit"
                size="lg"
                disabled={request.kind === "loading"}
                className="sm:w-28"
              >
                {request.kind === "loading" ? (
                  <RotateCcw className="animate-spin" aria-hidden="true" />
                ) : (
                  <Search aria-hidden="true" />
                )}
                {request.kind === "loading" ? "Searching" : "Search"}
              </Button>
            </div>
            <div className="text-muted-foreground mt-2 flex min-h-5 items-start justify-between gap-4 text-[0.7rem]">
              <p id="query-help">
                2–160 characters. Press <span className="font-mono">/</span> to
                focus.
              </p>
              <p
                id="query-error"
                role="alert"
                className="text-destructive text-right font-medium"
              >
                {validationMessage}
              </p>
            </div>
          </form>

          <div
            className="mt-5 flex flex-wrap items-center gap-2"
            aria-label="Suggested searches"
          >
            <span className="text-muted-foreground mr-1 font-mono text-[0.64rem] font-semibold tracking-[0.08em] uppercase">
              Try
            </span>
            {suggestedQueries.map((suggestion) => (
              <button
                key={suggestion.label}
                type="button"
                onClick={() => chooseSuggestion(suggestion)}
                className="border-border bg-background/70 text-muted-foreground hover:border-primary/30 hover:bg-primary/7 hover:text-foreground focus-visible:ring-ring/30 rounded-full border px-3 py-1.5 text-xs font-medium transition-colors focus-visible:ring-3 focus-visible:outline-none"
              >
                {suggestion.label}
              </button>
            ))}
          </div>
        </CardContent>

        <div className="border-border/70 bg-muted/18 border-t">
          <div className="border-border/70 flex min-h-12 items-center justify-between gap-4 border-b px-5 py-3 sm:px-6">
            <p className="text-sm font-semibold" aria-live="polite">
              <ResultSummary request={request} />
            </p>
            {resultData && (
              <span className="text-muted-foreground inline-flex shrink-0 items-center gap-1.5 font-mono text-[0.67rem]">
                <Clock3 className="size-3.5" aria-hidden="true" />{" "}
                {resultData.elapsed_ms} ms engine
              </span>
            )}
          </div>

          <div
            className="grid gap-3 p-3 sm:p-4"
            aria-busy={request.kind === "loading"}
          >
            {request.kind === "idle" && (
              <EmptyPanel
                icon={<Sparkles />}
                title="Ready for a question"
                body="Search the 24-page reviewed sample corpus."
              />
            )}
            {request.kind === "loading" && <LoadingResults />}
            {request.kind === "error" && (
              <ErrorPanel
                error={request.error}
                onRetry={() => void executeSearch(query, source)}
              />
            )}
            {resultData?.results.length === 0 && (
              <EmptyPanel
                icon={<Search />}
                title="No exact result"
                body="Try fewer terms or search all sources."
              />
            )}
            {resultData?.results.map((result, index) => (
              <ResultCard
                key={`${result.source}:${result.path}`}
                result={result}
                rank={index + 1}
                query={resultData.query}
                onPreview={() => void openDocument(result)}
              />
            ))}
          </div>
        </div>

        <CardFooter className="flex-col items-stretch gap-3 bg-[#101827] px-5 py-4 text-slate-300 sm:flex-row sm:items-center sm:justify-between sm:px-6">
          <code className="min-w-0 truncate font-mono text-[0.68rem]">
            <span className="text-emerald-400">$</span> {command}
          </code>
          <Button
            type="button"
            variant="ghost"
            size="sm"
            onClick={() => void copyCommand()}
            className="shrink-0 text-slate-300 hover:bg-white/8 hover:text-white"
          >
            {copied ? (
              <Check aria-hidden="true" />
            ) : (
              <Clipboard aria-hidden="true" />
            )}
            {copied ? "Copied" : "Copy CLI command"}
          </Button>
        </CardFooter>
      </Card>

      <aside className="grid content-start gap-3" aria-label="Demo facts">
        <FactCard icon={<ShieldCheck />} label="Privacy boundary">
          No account, cookie, browser token, local path, or raw-query telemetry.
        </FactCard>
        <FactCard icon={<Code2 />} label="Canonical engine">
          <span className="font-mono">
            docs-puller {meta?.engine.version ?? "…"}
          </span>{" "}
          with SQLite FTS5. No browser reimplementation.
        </FactCard>
        <FactCard icon={<FileText />} label="Corpus identity">
          <strong className="text-foreground font-semibold">
            24 public pages
          </strong>{" "}
          from SQLite, Go, and PostgreSQL.
          {meta && (
            <span className="text-muted-foreground mt-2 block font-mono text-[0.62rem]">
              {meta.corpus.digest.slice(0, 18)}…
            </span>
          )}
        </FactCard>
        <a
          href="/method/"
          className="group border-border/70 bg-background/55 hover:border-primary/30 hover:bg-primary/6 flex items-center justify-between rounded-xl border p-4 text-sm font-semibold transition-colors"
        >
          Audit the method
          <ArrowUpRight
            className="text-muted-foreground group-hover:text-primary size-4 transition-transform group-hover:translate-x-0.5 group-hover:-translate-y-0.5"
            aria-hidden="true"
          />
        </a>
      </aside>

      <Dialog
        open={selectedResult !== null}
        onOpenChange={(open) => {
          if (!open) {
            documentAbortRef.current?.abort()
            setSelectedResult(null)
            setDocument({ kind: "idle" })
          }
        }}
      >
        <DialogContent>
          <DialogHeader>
            <div className="mb-1 flex items-center gap-2">
              <Badge variant="signal">
                {selectedResult
                  ? sourceLabels[selectedResult.source]
                  : "Document"}
              </Badge>
              <span className="text-muted-foreground font-mono text-[0.65rem]">
                plain Markdown preview
              </span>
            </div>
            <DialogTitle>
              {selectedResult?.title ?? "Document preview"}
            </DialogTitle>
            <DialogDescription>{selectedResult?.path}</DialogDescription>
          </DialogHeader>
          <div className="border-border min-h-64 overflow-auto border-y bg-[#101827] p-5 sm:p-6">
            {document.kind === "loading" && (
              <div className="space-y-3" aria-label="Loading document">
                <Skeleton className="h-5 w-2/3 bg-white/10" />
                <Skeleton className="h-3 w-full bg-white/8" />
                <Skeleton className="h-3 w-5/6 bg-white/8" />
                <Skeleton className="h-3 w-11/12 bg-white/8" />
              </div>
            )}
            {document.kind === "error" && (
              <p className="text-sm text-red-300">
                {messageForError(document.error)}
              </p>
            )}
            {document.kind === "success" && (
              <pre className="font-mono text-[0.72rem] leading-relaxed whitespace-pre-wrap text-slate-300">
                {document.data.content}
              </pre>
            )}
          </div>
          <DialogFooter>
            <span className="text-muted-foreground font-mono text-[0.65rem]">
              {document.kind === "success"
                ? `${document.data.bytes.toLocaleString()} bytes · rendered as text`
                : "Content never executes"}
            </span>
            {selectedResult && (
              <a
                href={selectedResult.url}
                target="_blank"
                rel="noreferrer"
                className="text-primary inline-flex items-center gap-1.5 text-sm font-semibold hover:underline hover:underline-offset-4"
              >
                Open canonical source{" "}
                <ArrowUpRight className="size-3.5" aria-hidden="true" />
              </a>
            )}
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  )
}

function ResultSummary({ request }: { request: RequestState }) {
  switch (request.kind) {
    case "idle":
      return "Search results"
    case "loading":
      return "Searching the FTS5 index…"
    case "error":
      return "The search did not complete"
    case "success":
      return `${request.data.result_count} ${request.data.result_count === 1 ? "result" : "results"} from the public sample`
  }
}

function ResultCard({
  result,
  rank,
  query,
  onPreview,
}: {
  result: SearchResult
  rank: number
  query: string
  onPreview(): void
}) {
  return (
    <article className="group border-border/75 bg-card hover:border-primary/28 rounded-xl border p-4 shadow-[0_1px_0_rgba(255,255,255,.45)_inset] transition-[border-color,transform,box-shadow] hover:-translate-y-0.5 hover:shadow-lg sm:p-5">
      <div className="flex gap-3.5">
        <span className="bg-muted text-muted-foreground group-hover:bg-primary/10 group-hover:text-primary mt-0.5 flex size-7 shrink-0 items-center justify-center rounded-lg font-mono text-[0.65rem] font-semibold">
          {String(rank).padStart(2, "0")}
        </span>
        <div className="min-w-0 flex-1">
          <div className="flex flex-col gap-2 sm:flex-row sm:items-start sm:justify-between">
            <div className="min-w-0">
              <Badge variant="outline" className="mb-2">
                {sourceLabels[result.source]}
              </Badge>
              <h3 className="truncate text-[0.98rem] font-semibold tracking-[-0.022em]">
                {result.title}
              </h3>
              <p className="text-muted-foreground mt-1 truncate font-mono text-[0.65rem]">
                {result.path}
              </p>
            </div>
            <div className="flex shrink-0 items-center gap-1">
              <Button
                type="button"
                variant="ghost"
                size="sm"
                onClick={onPreview}
              >
                <FileText aria-hidden="true" /> Preview
              </Button>
              <Button
                variant="ghost"
                size="icon-sm"
                render={
                  <a
                    href={result.url}
                    target="_blank"
                    rel="noreferrer"
                    aria-label={`Open ${result.title} at its canonical source`}
                  />
                }
              >
                <ArrowUpRight aria-hidden="true" />
              </Button>
            </div>
          </div>
          {result.snippets.length > 0 && (
            <div className="border-primary/18 mt-4 space-y-2 border-l-2 pl-3.5">
              {result.snippets.slice(0, 2).map((snippet) => (
                <p
                  key={`${snippet.line}:${snippet.text}`}
                  className="text-muted-foreground text-sm leading-relaxed"
                >
                  <span className="text-primary/75 mr-2 font-mono text-[0.61rem]">
                    L{snippet.line}
                  </span>
                  <Highlight text={snippet.text} query={query} />
                </p>
              ))}
            </div>
          )}
        </div>
      </div>
    </article>
  )
}

function Highlight({ text, query }: { text: string; query: string }) {
  const tokens = Array.from(
    new Set(
      normalizeQuery(query)
        .split(" ")
        .map((token) => token.toLocaleLowerCase())
        .filter((token) => token.length >= 3)
    )
  )
  if (tokens.length === 0) return text
  const expression = new RegExp(
    `(${tokens.map(escapeRegExp).join("|")})`,
    "giu"
  )
  return text.split(expression).map((part, index) =>
    tokens.includes(part.toLocaleLowerCase()) ? (
      <mark
        key={`${part}:${index}`}
        className="bg-primary/12 text-foreground rounded-sm px-0.5"
      >
        {part}
      </mark>
    ) : (
      part
    )
  )
}

function LoadingResults() {
  return (
    <div className="grid gap-3" aria-label="Loading search results">
      {[0, 1, 2].map((index) => (
        <div
          key={index}
          className="border-border/70 bg-card rounded-xl border p-5"
        >
          <div className="flex gap-4">
            <Skeleton className="size-7 shrink-0" />
            <div className="w-full space-y-3">
              <Skeleton className="h-4 w-24" />
              <Skeleton className="h-5 w-2/3" />
              <Skeleton className="h-3 w-full" />
            </div>
          </div>
        </div>
      ))}
    </div>
  )
}

function EmptyPanel({
  icon,
  title,
  body,
}: {
  icon: ReactNode
  title: string
  body: string
}) {
  return (
    <div className="border-border bg-background/45 flex min-h-52 flex-col items-center justify-center rounded-xl border border-dashed p-8 text-center">
      <span className="bg-muted text-muted-foreground flex size-10 items-center justify-center rounded-xl [&>svg]:size-5">
        {icon}
      </span>
      <p className="mt-4 text-sm font-semibold">{title}</p>
      <p className="text-muted-foreground mt-1 max-w-sm text-xs leading-relaxed">
        {body}
      </p>
    </div>
  )
}

function ErrorPanel({ error, onRetry }: { error: Error; onRetry(): void }) {
  return (
    <div className="border-destructive/20 bg-destructive/[.035] flex min-h-52 flex-col items-center justify-center rounded-xl border p-8 text-center">
      <span className="bg-destructive/10 text-destructive flex size-10 items-center justify-center rounded-xl">
        <RotateCcw className="size-5" aria-hidden="true" />
      </span>
      <p className="mt-4 text-sm font-semibold">
        Search is temporarily unavailable
      </p>
      <p className="text-muted-foreground mt-1 max-w-md text-xs leading-relaxed">
        {messageForError(error)}
      </p>
      <Button
        type="button"
        variant="outline"
        size="sm"
        onClick={onRetry}
        className="mt-5"
      >
        Try again
      </Button>
    </div>
  )
}

function FactCard({
  icon,
  label,
  children,
}: {
  icon: ReactNode
  label: string
  children: ReactNode
}) {
  return (
    <div className="border-border/70 bg-background/55 rounded-xl border p-4">
      <div className="text-primary flex items-center gap-2 [&>svg]:size-4">
        {icon}
        <p className="font-mono text-[0.64rem] font-semibold tracking-[0.08em] uppercase">
          {label}
        </p>
      </div>
      <p className="text-muted-foreground mt-3 text-xs leading-relaxed">
        {children}
      </p>
    </div>
  )
}

function parseSource(value: string | null): SourceID | "" {
  return value === "sqlite" || value === "go" || value === "postgresql"
    ? value
    : ""
}

function messageForError(error: Error): string {
  if (!(error instanceof DemoAPIError)) {
    return "The network request failed. Your query was not stored. Try again in a moment."
  }
  switch (error.code) {
    case "rate_limited":
      return `The public request budget is full. Try again${error.retryAfterSeconds ? ` in ${error.retryAfterSeconds} seconds` : " shortly"}.`
    case "origin_timeout":
      return "The origin did not answer within four seconds. Your query was not stored."
    case "origin_unavailable":
    case "origin_invalid":
      return "The Go search origin is not ready. The edge service did not return stale or unverified data."
    case "invalid_request":
      return "The query did not match the bounded public contract. Use 2 to 160 characters."
    default:
      return error.message
  }
}

function escapeRegExp(value: string): string {
  return value.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")
}

function asError(value: unknown): Error {
  return value instanceof Error ? value : new Error("Unknown demo error")
}
