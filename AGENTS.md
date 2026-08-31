# Repository Guidelines

## Project Structure & Module Organization

The backend entry is `cmd/server/`; core code is in `internal/`, configuration in `config/`, and migrations in `migrations/`. The CLI (`cli/`) and SDK (`client/`) are separate Go modules. Vue 3 code lives in `frontend/src/`, organized into components, composables, stores, views, and assets. Parsing code is in `docreader/`; deployment resources are in `docker/`, `helm/`, and the root Compose files. Put documentation in `docs/` or `website-docs/` and shared fixtures in `testdata/`.

## Build, Test, and Development Commands

Use Go 1.26, Node 24, npm, Make, and Docker with Compose. Run `cp .env.example .env`, then `make dev-start`; launch `make dev-app` and `make dev-frontend` in separate terminals. Stop services with `make dev-stop`.

- `make build`: compile the server to `./WeKnora`.
- `make test`: run tests for the root Go module.
- `make lint`: run Go lint checks.
- `make -C cli test`: test the CLI module.
- `cd client && go test ./...`: test the Go SDK.
- `cd frontend && npm ci && npm test`: install locked dependencies and test the frontend.
- `cd frontend && npm run type-check && npm run build`: validate and build the frontend.

## Coding Style & Naming Conventions

Format Go files with `gofmt`; CI also checks `gofumpt`, `govet`, `revive`, and a 120-character line limit. Use `PascalCase` for exported Go identifiers, `camelCase` for unexported identifiers, and lowercase package names. In Vue/TypeScript, follow surrounding two-space formatting, name components `PascalCase.vue`, and composables `useSomething`. Keep component styles scoped and reuse existing components, stores, and utilities.

## Testing Guidelines

Name Go tests `*_test.go` and frontend tests `*.test.ts`. Keep tests beside their source. Run focused tests first, then the relevant module suite. There is no global coverage threshold, but changed behavior should be tested. Document infrastructure-dependent failures in the pull request.

When an approved behavior or permission contract changes, update the directly affected existing test assertions in the same change. Do not leave a known failing test or broaden production behavior merely to satisfy a stale expectation. This does not authorize unrelated test refactors or new test files.

## Commit & Pull Request Guidelines

Use Conventional Commits, optionally with a scope: `feat(chat): add source filter`, `fix: handle empty upload`, or `docs: clarify setup`. Keep commits focused. Pull requests must explain the change, link issues with `Fixes #123` when applicable, list validation commands, and note failures. Complete `.github/pull_request_template.md`, call out breaking changes, and include screenshots or recordings for UI changes.

## Security & Configuration

Copy `.env.example` for local configuration and never commit `.env`, credentials, keys, or generated data. Report vulnerabilities privately through GitHub Security Advisories rather than public issues.
