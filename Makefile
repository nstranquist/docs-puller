# Local developer gates for the docs-puller open-core CLI.
# CI is defined in .github/workflows/ci.yml (gofmt, build, vet, test, vscode).

.PHONY: help build test vet fmt verify publish-ready install smoke version help-sizes

GOFLAGS := -tags sqlite_fts5

help:
	@echo "docs-puller local targets:"
	@echo "  make build | test | vet | fmt | verify | publish-ready"
	@echo "  make install | smoke | version | help-sizes"

# Alias: local publication gate (no remote push).
publish-ready: verify

build:
	go build $(GOFLAGS) -o bin/docs-puller .

test:
	go test $(GOFLAGS) ./...

vet:
	go vet $(GOFLAGS) ./...

fmt:
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then \
		echo "gofmt needed on:" >&2; \
		echo "$$unformatted" >&2; \
		exit 1; \
	fi

verify: fmt vet test build

install: build
	go install $(GOFLAGS) .

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
	mkdir -p "$$tmp/input"; \
	printf '# PurpleWidget setup\n\nRun `purplewidget init`.\n' > "$$tmp/input/setup.md"; \
	./bin/docs-puller pull --local "$$tmp/input" --name smoke --out "$$tmp/corpus"; \
	./bin/docs-puller reindex --out "$$tmp/corpus"; \
	./bin/docs-puller status --out "$$tmp/corpus" --check; \
	./bin/docs-puller search "purplewidget init" --out "$$tmp/corpus" --source smoke --limit 1 --json; \
	echo "smoke ok ($$tmp)"
