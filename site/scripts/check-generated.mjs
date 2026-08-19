import { spawnSync } from "node:child_process"
import { readFileSync, rmSync } from "node:fs"
import { resolve } from "node:path"

const root = resolve(import.meta.dirname, "..")
const committed = resolve(root, "src/lib/api/generated.ts")
const candidate = resolve(root, ".generated-api.ts")

const result = spawnSync(
  process.execPath,
  [
    resolve(root, "node_modules/openapi-typescript/bin/cli.js"),
    resolve(root, "openapi/demo-v1.yaml"),
    "-o",
    candidate,
  ],
  { cwd: root, encoding: "utf8" }
)

if (result.status !== 0) {
  process.stderr.write(result.stderr)
  process.exit(result.status ?? 1)
}

try {
  if (readFileSync(candidate, "utf8") !== readFileSync(committed, "utf8")) {
    process.stderr.write(
      "Generated API types are stale. Run `pnpm generate:api` and commit the result.\n"
    )
    process.exitCode = 1
  }
} finally {
  rmSync(candidate, { force: true })
}
