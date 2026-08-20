# Local developer and release gates for the docs-puller open-core CLI.

.PHONY: help build test test-race vet fmt staticcheck vulncheck secret-scan \
	verify publish-ready install smoke demo-smoke version help-sizes fuzz-smoke \
	extension-check extension-package site-check verify-public-snapshot verify-public-sample verify-held-out \
	verify-retrieval-regression stage-public-demo \
	release-check release-sync-check release-sync-write release-dist \
	release-verify release-ready verify-clean-clone

GO_TAG_FLAGS := -tags sqlite_fts5
STATICCHECK_VERSION ?= v0.7.0
GOVULNCHECK_VERSION ?= v1.1.4
FUZZ_SMOKE_BUDGET ?= 10000x
RELEASE_VERSION ?=
RELEASE_VERSION_ARG := $(if $(RELEASE_VERSION),--version $(RELEASE_VERSION),)
NDEV_ROOT ?=
PUBLIC_SAMPLE_MIN_HIT_AT_1 ?= 0.90
PUBLIC_SAMPLE_MIN_HIT_AT_5 ?= 1.0
PUBLIC_SAMPLE_MIN_MRR ?= 0.95
PUBLIC_SAMPLE_MAX_P99_MS ?= 250
HELD_OUT_CORPUS ?= $(HOME)/code/docs
HELD_OUT_FIXTURE ?= eval/heldout-v0.6-final.yaml
HELD_OUT_MIN_HIT_AT_1 ?= 0.45
HELD_OUT_MIN_HIT_AT_5 ?= 0.9001
HELD_OUT_MIN_MRR ?= 0.65
# Full-corpus cold reads can pay filesystem cache cost; the immutable audit
# recorded 191ms p99 warm and 1.56s p99 cold. Keep a 2s fail-closed ceiling.
HELD_OUT_MAX_P99_MS ?= 2000
REGRESSION_FIXTURE ?= eval/fixture.yaml
REGRESSION_MIN_HIT_AT_5 ?= 1.0

help:
	@echo "docs-puller local targets:"
	@echo "  make build | test | vet | fmt | verify | publish-ready"
	@echo "  make install | smoke | demo-smoke | version | help-sizes"
	@echo "  make site-check"
	@echo "  make verify-public-snapshot | verify-public-sample"
	@echo "  make stage-public-demo"
	@echo "  make verify-retrieval-regression | verify-held-out"
	@echo "  make verify-clean-clone"
	@echo "  make release-check | release-dist | release-verify | release-ready"
	@echo "  make release-sync-check NDEV_ROOT=/path/to/nicos-tools"

# Complete local publication gate. It does not push or publish.
publish-ready: verify help-sizes smoke test-race staticcheck vulncheck secret-scan fuzz-smoke extension-package site-check verify-public-snapshot

build:
	go build $(GO_TAG_FLAGS) -o bin/docs-puller .

test:
	go test $(GO_TAG_FLAGS) ./...

test-race:
	go test -race $(GO_TAG_FLAGS) ./...

vet:
	go vet $(GO_TAG_FLAGS) ./...

fmt:
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then \
		echo "gofmt needed on:" >&2; \
		echo "$$unformatted" >&2; \
		exit 1; \
	fi

staticcheck:
	GOWORK=off go run honnef.co/go/tools/cmd/staticcheck@$(STATICCHECK_VERSION) $(GO_TAG_FLAGS) ./...

vulncheck:
	GOWORK=off go run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) $(GO_TAG_FLAGS) ./...

secret-scan:
	@command -v gitleaks >/dev/null 2>&1 || { echo "gitleaks is required" >&2; exit 1; }
	gitleaks dir . --no-banner --redact
	gitleaks git . --no-banner --redact

verify: fmt vet test build demo-smoke extension-check

install: build
	go install $(GO_TAG_FLAGS) .

version: build
	./bin/docs-puller version --json

# Fail if compact help is not substantially smaller than full help.
help-sizes: build
	@full=$$(./bin/docs-puller help 2>&1 | wc -c | tr -d ' '); \
	compact=$$(./bin/docs-puller help --compact 2>/dev/null | wc -c | tr -d ' '); \
	echo "full=$$full compact=$$compact"; \
	test "$$compact" -gt 0; \
	test "$$compact" -lt $$((full / 3))

# Tiny isolated corpus — no network, no HOME corpus mutation.
smoke: build
	@tmp=$$(mktemp -d); \
	trap 'rm -rf -- "$$tmp"' EXIT; \
	mkdir -p "$$tmp/input"; \
	printf '# PurpleWidget setup\n\nRun `purplewidget init`.\n' > "$$tmp/input/setup.md"; \
	./bin/docs-puller pull --local "$$tmp/input" --name smoke --out "$$tmp/corpus"; \
	./bin/docs-puller reindex --out "$$tmp/corpus"; \
	./bin/docs-puller status --out "$$tmp/corpus" --check; \
	./bin/docs-puller search "purplewidget init" --out "$$tmp/corpus" --source smoke --limit 1 --json; \
	echo "smoke ok"

# Built-in isolated demo — no network and no normal corpus mutation.
demo-smoke: build
	./bin/docs-puller demo --json

fuzz-smoke:
	go test $(GO_TAG_FLAGS) -run '^$$' -fuzz '^FuzzDedupeLocaleVariants$$' -fuzztime $(FUZZ_SMOKE_BUDGET) .
	go test $(GO_TAG_FLAGS) -run '^$$' -fuzz '^FuzzManifestParse$$' -fuzztime $(FUZZ_SMOKE_BUDGET) .
	go test $(GO_TAG_FLAGS) -run '^$$' -fuzz '^FuzzFtsBuildQuery$$' -fuzztime $(FUZZ_SMOKE_BUDGET) .
	go test $(GO_TAG_FLAGS) -run '^$$' -fuzz '^FuzzConfigParse$$' -fuzztime $(FUZZ_SMOKE_BUDGET) ./internal/userconfig

extension-check:
	npm ci --prefix vscode-extension
	npm run check --prefix vscode-extension
	npm test --prefix vscode-extension
	npm audit --prefix vscode-extension --audit-level=high

extension-package: extension-check
	npm run package --prefix vscode-extension
	npm run package --prefix vscode-extension

site-check:
	@command -v pnpm >/dev/null 2>&1 || { echo "pnpm is required" >&2; exit 1; }
	pnpm --dir site install --frozen-lockfile
	pnpm --dir site run check
	pnpm --dir site exec playwright install chromium
	pnpm --dir site run test:e2e
	pnpm --dir site audit --audit-level high
	pnpm --dir site run deploy:dry-run

# Live, key-free replay of the public 24-query fixture.
verify-public-sample: build
	@sample_root=$$(mktemp -d); \
	trap 'rm -rf -- "$$sample_root"' EXIT; \
	./bin/docs-puller pull --from eval/sample-corpus/sources.md --out "$$sample_root/corpus"; \
	./bin/docs-puller reindex --out "$$sample_root/corpus"; \
	./bin/docs-puller eval --fixture eval/sample-corpus/fixture.yaml --out "$$sample_root/corpus" --json \
		--min-hit-at-1 $(PUBLIC_SAMPLE_MIN_HIT_AT_1) \
		--min-hit-at-5 $(PUBLIC_SAMPLE_MIN_HIT_AT_5) \
		--min-mrr $(PUBLIC_SAMPLE_MIN_MRR) \
		--max-p99-ms $(PUBLIC_SAMPLE_MAX_P99_MS)

# Offline proof that the reviewed production snapshot creates one exact index.
verify-public-snapshot: build
	@snapshot_root=$$(mktemp -d); \
	trap 'rm -rf -- "$$snapshot_root"' EXIT; \
	mkdir -p "$$snapshot_root/a" "$$snapshot_root/b"; \
	cp -R deploy/demo/snapshot/. "$$snapshot_root/a/"; \
	cp -R deploy/demo/snapshot/. "$$snapshot_root/b/"; \
	./bin/docs-puller reindex --out "$$snapshot_root/a"; \
	./bin/docs-puller reindex --out "$$snapshot_root/b"; \
	go run $(GO_TAG_FLAGS) ./deploy/demo/cmd/corpus-builder --corpus "$$snapshot_root/a" > "$$snapshot_root/a.json"; \
	go run $(GO_TAG_FLAGS) ./deploy/demo/cmd/corpus-builder --corpus "$$snapshot_root/b" > "$$snapshot_root/b.json"; \
	cmp "$$snapshot_root/a/.cache/search.db" "$$snapshot_root/b/.cache/search.db"; \
	index_a=$$(jq -er .index_digest "$$snapshot_root/a.json"); \
	index_b=$$(jq -er .index_digest "$$snapshot_root/b.json"); \
	test "$$index_a" = "$$index_b"; \
	./bin/docs-puller status --out "$$snapshot_root/a" --check; \
	DOCS_PULLER_QUERY_LOG=0 ./bin/docs-puller eval \
		--fixture eval/sample-corpus/fixture.yaml \
		--out "$$snapshot_root/a" --json \
		--min-hit-at-1 $(PUBLIC_SAMPLE_MIN_HIT_AT_1) \
		--min-hit-at-5 $(PUBLIC_SAMPLE_MIN_HIT_AT_5) \
		--min-mrr $(PUBLIC_SAMPLE_MIN_MRR) \
		--max-p99-ms $(PUBLIC_SAMPLE_MAX_P99_MS) > "$$snapshot_root/eval.json"; \
	jq -n --arg index_digest "$$index_a" \
		--arg corpus_digest "$$(jq -er .corpus_digest "$$snapshot_root/a.json")" \
		--slurpfile evaluation "$$snapshot_root/eval.json" \
		'{ok:true, corpus_digest:$$corpus_digest, index_digest:$$index_digest, evaluation:$$evaluation[0].summary}'

# Create the exact, ignored container context used for a manual deployment.
stage-public-demo: verify-public-snapshot
	@stage_root=$$(mktemp -d); \
	trap 'rm -rf -- "$$stage_root"' EXIT; \
	mkdir -p "$$stage_root/corpus" "$$stage_root/bin"; \
	cp -R deploy/demo/snapshot/. "$$stage_root/corpus/"; \
	./bin/docs-puller reindex --out "$$stage_root/corpus"; \
	version=$$(jq -er .version release/manifest.json); \
	commit=$$(git rev-parse HEAD); \
	ldflags="-s -w -buildid= -X main.releaseIdentity=docs-puller-release:$$version@$$commit"; \
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -mod=readonly $(GO_TAG_FLAGS) \
		-trimpath -buildvcs=false -ldflags "$$ldflags" -o "$$stage_root/bin/docs-puller-a" .; \
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -mod=readonly $(GO_TAG_FLAGS) \
		-trimpath -buildvcs=false -ldflags "$$ldflags" -o "$$stage_root/bin/docs-puller-b" .; \
	cmp "$$stage_root/bin/docs-puller-a" "$$stage_root/bin/docs-puller-b"; \
	go run $(GO_TAG_FLAGS) ./deploy/demo/cmd/corpus-builder \
		--corpus "$$stage_root/corpus" \
		--binary "$$stage_root/bin/docs-puller-a" \
		--build-context deploy/demo/.build

# Private/local held-out gate. The fixture is public; the corpus can be private.
verify-held-out: build
	./bin/docs-puller status --out "$(HELD_OUT_CORPUS)" --stale-days 0 --check
	./bin/docs-puller eval --fixture "$(HELD_OUT_FIXTURE)" --out "$(HELD_OUT_CORPUS)" --json \
		--min-hit-at-1 $(HELD_OUT_MIN_HIT_AT_1) \
		--min-hit-at-5 $(HELD_OUT_MIN_HIT_AT_5) \
		--min-mrr $(HELD_OUT_MIN_MRR) \
		--max-p99-ms $(HELD_OUT_MAX_P99_MS)

# Tuned regression corpus. Keep this distinct from the frozen generalization holdout.
verify-retrieval-regression: build
	./bin/docs-puller status --out "$(HELD_OUT_CORPUS)" --stale-days 0 --check
	./bin/docs-puller eval --fixture "$(REGRESSION_FIXTURE)" --out "$(HELD_OUT_CORPUS)" --json \
		--min-hit-at-5 $(REGRESSION_MIN_HIT_AT_5)

release-check:
	go run ./cmd/release-tool check $(RELEASE_VERSION_ARG) --json

release-sync-check:
	@test -n "$(NDEV_ROOT)" || { echo "NDEV_ROOT is required" >&2; exit 1; }
	go run ./cmd/release-tool sync $(RELEASE_VERSION_ARG) --ndev-root "$(NDEV_ROOT)" --json

release-sync-write:
	@test -n "$(NDEV_ROOT)" || { echo "NDEV_ROOT is required" >&2; exit 1; }
	go run ./cmd/release-tool sync $(RELEASE_VERSION_ARG) --ndev-root "$(NDEV_ROOT)" --write --json

release-dist:
	go run ./cmd/release-tool dist $(RELEASE_VERSION_ARG) --json

release-verify: release-dist
	go run ./cmd/release-tool verify $(RELEASE_VERSION_ARG) --json

release-ready: publish-ready release-check verify-public-sample verify-retrieval-regression verify-held-out release-verify verify-clean-clone

# Proves committed source from a fresh clone, excluding local generated state.
verify-clean-clone:
	@clone_root=$$(mktemp -d); \
	trap 'rm -rf -- "$$clone_root"' EXIT; \
	git clone --quiet --no-hardlinks . "$$clone_root/docs-puller"; \
	$(MAKE) -C "$$clone_root/docs-puller" publish-ready
