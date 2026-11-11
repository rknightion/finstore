set shell := ["bash", "-euo", "pipefail", "-c"]

version := `git describe --tags --always --dirty 2>/dev/null || echo dev`
commit := `git rev-parse HEAD 2>/dev/null || echo unknown`
build_date := `date -u +%Y-%m-%dT%H:%M:%SZ`
ldflags := "-s -w -X main.version=" + version + " -X main.commit=" + commit + " -X main.buildDate=" + build_date
tools_dir := justfile_directory() + "/.tools"

# renovate: datasource=go depName=github.com/golangci/golangci-lint/v2
golangci_lint_version := "v2.15.0"
# renovate: datasource=go depName=golang.org/x/vuln
govulncheck_version := "v1.9.0"
golangci_lint_dir := tools_dir + "/golangci-lint-" + golangci_lint_version

# List available recipes
default:
    @just --list </dev/null

# Install pinned tools and download modules
setup: _install-golangci-lint
    go mod download

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

# The pre-commit gate: everything that runs with the Go toolchain and PostgreSQL binaries
[group('check')]
check: fmt-check lint vet test tidy-check build vuln

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
