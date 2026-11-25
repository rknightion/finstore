set shell := ["bash", "-euo", "pipefail", "-c"]

version := `git describe --tags --always --dirty 2>/dev/null || echo dev`
commit := `git rev-parse HEAD 2>/dev/null || echo unknown`
build_date := `date -u +%Y-%m-%dT%H:%M:%SZ`
ldflags := "-s -w -X main.version=" + version + " -X main.commit=" + commit + " -X main.buildDate=" + build_date
tools_dir := justfile_directory() + "/.tools"

# renovate: datasource=go depName=github.com/golangci/golangci-lint/v2
golangci_lint_version := "v2.15.0"
# renovate: datasource=go depName=golang.org/x/vuln
govulncheck_version := "v1.10.0"
golangci_lint_dir := tools_dir + "/golangci-lint-" + golangci_lint_version
# renovate: datasource=go depName=github.com/k1LoW/tbls
tbls_version := "v1.97.0"
tbls_dir := tools_dir + "/tbls-" + tbls_version

# List available recipes
default:
    @just --list </dev/null

# Install pinned tools and download modules
setup: _install-golangci-lint
    go mod download

# Prepare the tools and compile the race-enabled test binaries without running them.
cloud-bootstrap: setup
    GOBIN={{ tools_dir }} go install golang.org/x/vuln/cmd/govulncheck@{{ govulncheck_version }}
    scratch="$(mktemp -d)"; trap 'rm -rf "$scratch"' EXIT; go test -c -race -o "$scratch/" ./...

[private]
_install-golangci-lint:
    mkdir -p {{ golangci_lint_dir }}
    test -x {{ golangci_lint_dir }}/golangci-lint || GOBIN={{ golangci_lint_dir }} go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@{{ golangci_lint_version }}

# Format Go sources in place
[group('dev')]
fmt:
    gofmt -w $(find . -name '*.go' -not -path './vendor/*' -not -path './.tools/*')

# Fail if Go sources or the justfile need formatting
[group('check')]
[no-exit-message]
fmt-check:
    files="$(gofmt -l $(find . -name '*.go' -not -path './vendor/*' -not -path './.tools/*'))"; test -z "$files" || { echo "Go files require formatting:"; echo "$files"; exit 1; }
    just --fmt --check </dev/null

# Run golangci-lint
[group('check')]
[no-exit-message]
lint: _install-golangci-lint
    {{ golangci_lint_dir }}/golangci-lint run --allow-parallel-runners ./...

# Run go vet
[group('check')]
[no-exit-message]
vet:
    go vet ./...

# Run the race-enabled test suite, optionally filtered by test name. Needs the PostgreSQL 18 server binaries (initdb, pg_ctl, postgres) on PATH.
[group('check')]
[no-exit-message]
test filter="":
    if [ -n "{{ filter }}" ]; then go test -race -run '{{ filter }}' ./...; else go test -race ./...; fi

# Fail if go.mod or go.sum are untidy
[group('check')]
[no-exit-message]
tidy-check:
    go mod tidy -diff

# Tidy go.mod and go.sum
[group('dev')]
tidy:
    go mod tidy

# Build the binary into bin/
[group('build')]
build:
    go build -trimpath -ldflags "{{ ldflags }}" -o bin/finstore ./cmd/finstore

# Run govulncheck
[group('check')]
[no-exit-message]
vuln:
    go run golang.org/x/vuln/cmd/govulncheck@{{ govulncheck_version }} ./...

# Regenerate the database schema docs (tbls) from a freshly migrated, empty throwaway database. Needs the PostgreSQL 18 server binaries (initdb, pg_ctl, postgres) on PATH.
[group('gen')]
[no-exit-message]
schema-docs dest="docs/schema":
    #!/usr/bin/env bash
    set -euo pipefail
    for bin in initdb pg_ctl postgres createdb; do
        command -v "$bin" >/dev/null || { echo "schema-docs: $bin is not on PATH (PostgreSQL 18 server binaries are required)" >&2; exit 1; }
    done
    # A developer's PG* variables must not redirect the throwaway cluster or its utilities.
    for name in $(compgen -e | grep '^PG' || true); do unset "$name"; done
    mkdir -p {{ tbls_dir }}
    test -x {{ tbls_dir }}/tbls || GOBIN={{ tbls_dir }} go install github.com/k1LoW/tbls@{{ tbls_version }}
    work="$(mktemp -d)"
    cleanup() { pg_ctl -D "$work/data" -m immediate -w stop >/dev/null 2>&1 || true; rm -rf "$work"; }
    trap cleanup EXIT
    go build -o "$work/finstore" ./cmd/finstore
    # The cluster listens on loopback only, on a free port, with no Unix socket and trust
    # authentication. The passwords are throwaway values for a database that is deleted at the end.
    port=0
    for _ in $(seq 1 100); do
        candidate=$((20000 + RANDOM % 40000))
        if ! (exec 3<>"/dev/tcp/127.0.0.1/$candidate") 2>/dev/null; then port="$candidate"; break; fi
    done
    test "$port" -ne 0 || { echo "schema-docs: no free port found" >&2; exit 1; }
    initdb -D "$work/data" -A trust -U postgres --no-locale --encoding=UTF8 >/dev/null
    pg_ctl -D "$work/data" -l "$work/server.log" -w -t 60 -o "-c listen_addresses=127.0.0.1 -c port=$port -c unix_socket_directories= -c fsync=off" start >/dev/null
    createdb -h 127.0.0.1 -p "$port" -U postgres finstore
    export FINSTORE_SUPERUSER_DATABASE_URL="postgresql://postgres@127.0.0.1:$port/finstore?sslmode=disable"
    export FINSTORE_OWNER_DATABASE_URL="postgresql://finstore_owner:schemadocsowner1@127.0.0.1:$port/finstore?sslmode=disable"
    export FINSTORE_OWNER_PASSWORD=schemadocsowner1 FINSTORE_APP_PASSWORD=schemadocsapp1
    "$work/finstore" bootstrap </dev/null
    "$work/finstore" migrate </dev/null
    # tbls reads only the schemas on search_path, and splits the setting on ", ".
    export TBLS_DSN="postgres://finstore_owner:schemadocsowner1@127.0.0.1:$port/finstore?sslmode=disable&search_path=raw,%20core,%20agent,%20sync,%20secret"
    # Two passes: Mermaid diagrams inside the Markdown, and SVG files beside it. The SVG pass is
    # only mined for its .svg files.
    {{ tbls_dir }}/tbls doc --er-format svg --force --sort "$TBLS_DSN" "$work/svg" >/dev/null
    {{ tbls_dir }}/tbls doc --er-format mermaid --force --sort --rm-dist "$TBLS_DSN" {{ quote(dest) }} >/dev/null
    cp "$work"/svg/*.svg {{ quote(dest) }}/
    # tbls lists every name it finds in a view definition (CTE names, LATERAL, unqualified tables)
    # as a referenced table, with a link to a page that does not exist. Drop those rows: they are
    # the ones with no columns and no type.
    for page in {{ quote(dest) }}/*.md; do
        sed -E '/^\| \[[^]]+\]\([^)]+\.md\) \| 0 \|  \|  \|$/d' "$page" > "$work/page.md"
        mv "$work/page.md" "$page"
    done
    # The server version string names the build and platform; keep only the major version.
    sed -E 's/"database_version": "[^"]*"/"database_version": "PostgreSQL 18"/' {{ quote(dest) }}/schema.json > "$work/schema.json"
    mv "$work/schema.json" {{ quote(dest) }}/schema.json
    echo "schema docs written to {{ dest }}"

# Fail if docs/schema differs from what the current migrations and views produce. Needs the PostgreSQL 18 server binaries.
[group('check')]
[no-exit-message]
schema-docs-check:
    #!/usr/bin/env bash
    set -euo pipefail
    fresh="$(mktemp -d)"
    trap 'rm -rf "$fresh"' EXIT
    {{ just_executable() }} schema-docs "$fresh/schema" </dev/null
    diff -r docs/schema "$fresh/schema" || { echo "docs/schema is out of date: run 'just schema-docs' and commit the result" >&2; exit 1; }

# The pre-commit gate: everything that runs with the Go toolchain and PostgreSQL binaries
[group('check')]
check: fmt-check lint vet test tidy-check schema-docs-check build vuln

# Workstation feedback for Go edits since HEAD. Checks the working tree and its Go dependents.
# Unclear scope, shared files, and an empty change set use the full gate. Commit gates use `just check`.
[group('check')]
check-fast:
    if [ -n "${CI:-}${GITHUB_ACTIONS:-}" ] || ! command -v python3 >/dev/null; then just check </dev/null; else python3 tools/check_fast.py </dev/null; fi

# Build the container image
[group('build')]
image tag="finstore:dev":
    docker build --build-arg VERSION={{ version }} --build-arg COMMIT={{ commit }} --build-arg BUILD_DATE={{ build_date }} -t {{ tag }} .

# GitHub issues and the rknightion Project; maintainers only (needs the tracker tools and credentials)
[group('tracker')]
[positional-arguments]
tracker *args:
    #!/usr/bin/env bash
    set -euo pipefail
    export TRACKER_REPO=rknightion/finstore TRACKER_PROJECT_OWNER=rknightion TRACKER_PROJECT=7
    export TRACKER_ISSUE_IDENTITY=app TRACKER_PROJECT_IDENTITY=user
    exec tracker-with-token "$@"

# This repository has no legacy board ids; prints the issue number unchanged
[group('tracker')]
issue id:
    @echo {{ quote(id) }}

# Remove build output and downloaded tools
[group('build')]
clean:
    rm -rf bin .tools
