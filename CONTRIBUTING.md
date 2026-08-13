# Contributing to wan-ip-check

First off — thank you for considering a contribution. wan-ip-check is a small,
self-hosted tool that lives or dies by its **Actions**. The most valuable thing
you can build here is a new Action type that keeps someone else from running a
cron job.

By contributing you agree to the [Developer Certificate of Origin](#license-and-dco)
and that your contribution is licensed under the same [GNU GPL v3](LICENSE) as
the project. Please read that section before opening a PR.

- [Code of Conduct](#code-of-conduct)
- [Ways to contribute](#ways-to-contribute)
- [Development environment](#development-environment)
- [Adding a new Action type](#adding-a-new-action-type) (the primary path)
- [Coding standards](#coding-standards)
- [Testing](#testing)
- [Git workflow & commits](#git-workflow--commits)
- [Opening a pull request](#opening-a-pull-request)
- [License & DCO](#license--dco)

## Code of Conduct

This project adheres to the [Contributor Covenant](CODE_OF_CONDUCT.md). By
participating you are expected to uphold this code. Report unacceptable behavior
by opening an issue.

## Ways to contribute

**Report a bug.** Open an issue using the
[bug report template](https://github.com/AC13139/wan-ip-check/issues/new?template=bug_report.yml).
Include what you expected, what happened, the exact config involved, and logs.
The more concrete, the faster it gets fixed.

**Request a feature / new Action.** Open an issue using the
[feature request template](https://github.com/AC13139/wan-ip-check/issues/new?template=feature_request.yml).
New Action ideas are tracked under the
[`new-action` label](https://github.com/AC13139/wan-ip-check/labels/new-action) —
there's already a backlog of DDNS providers (DuckDNS, No-IP, Namecheap), SSH
execution, AWS Security Groups, and port-reachability checks. Claim one, or
suggest your own.

**Send a pull request.** The rest of this document is about making that go
smoothly.

## Development environment

Requirements:

- **Go 1.25+** (see `go.mod`)
- Docker / docker compose (optional — for the container path)
- No Node.js, no bundler, no frontend build step. The Web UI is Go `html/template`
  + HTMX, with CSS and HTMX embedded via `embed.FS`.

Run locally:

```bash
git clone git@github.com:ac13139/wan-ip-check.git
cd wan-ip-check
go build -o wan-ip-check .
./wan-ip-check          # Web UI + API on :7373, data/ created next to the binary
```

Or in Docker:

```bash
cp .env.example .env
docker compose up -d    # http://localhost:7373
```

Config lives in the DB, created through the Web UI — there is no YAML config to
edit. For automation testing, the REST API under `/api/*` is the fast path.

## Adding a new Action type

This is the main contribution surface. Action types are pluggable — adding one
does **not** require touching the UI, the API, the scheduler, or the history
tables, because the whole stack renders and executes from the registry.

### 1. Implement the `ActionExecutor` interface

Create `action_<type>.go` (see `action_cfdns.go`, `action_discord.go`,
`action_webhook.go`, `action_cfgateway.go` for patterns). The interface is
defined in `action.go`:

```go
type ActionExecutor interface {
    Type() string                       // stable internal ID, e.g. "duckdns"
    DisplayName() string                // e.g. "DuckDNS DDNS"
    Description() string                // one sentence for the Type dropdown
    Execute(ctx context.Context, event IPEvent, config json.RawMessage) error
    ValidateConfig(config json.RawMessage) error
    SecretFields() []string             // config fields to AES-GCM encrypt at rest
    DefaultConfig() json.RawMessage
    ConfigSchema() []ConfigField        // drives the auto-generated Web UI form
}
```

`IPEvent` carries the WAN name, event type (`ip_changed` / `ip_unchanged_daily`),
old/new IP, and timestamp. Your executor receives the **already-decrypted**
config in `Execute` — the framework handles encryption, masking, and retries.

### 2. Register it

In `main.go`, where the other executors are registered:

```go
registry.Register(&CFDNSExecutor{client: directClient})
registry.Register(&MyNewExecutor{client: directClient})
```

That's the whole wiring step.

### 3. The Web UI form comes free

The action form (`templates/action_form.html`) renders fields dynamically from
`ConfigSchema()`. Each `ConfigField` supports `text`, `password`, `textarea`,
`select`, and `tags` types, plus a `HelpText` for the inline hint. Only reach for
a custom partial (`templates/partials/`) if your config genuinely can't be
expressed as schema fields — and if you do, keep it consistent with the
existing partials in `templates/partials/` (same class names, same HTMX
patterns).

### 4. Secret handling

List every credential field in `SecretFields()`. The framework then:

- AES-256-GCM encrypts the value at rest (`data/*.db`), prefixed with `enc:`
- Masks it as `•••••` in every UI/API/MCP response and never returns it in
  plaintext
- Preserves the masked placeholder on re-save so you can't wipe a secret by
  saving the form

If your Action talks to a network service, honor the caller's `context.Context`
(with a timeout) and wrap returned errors with `%w`.

### 5. Tests

Create `action_<type>_test.go` with table-driven tests covering: config
validation (valid + each invalid shape), a successful `Execute` against a fake
HTTP server (`httptest`), and the failure path (server down / non-2xx). See
`action_cfdns_test.go` for the pattern. Always run with `-race`.

### 6. Documentation

- Add your Action to the **Action Types** list in `README.md`, with required
  permissions / preconditions and any notable behaviors.
- Add the file to the **Project Structure** tree in `README.md`.

## Coding standards

- **Go**: `gofmt` + `goimports` are mandatory. Keep functions small and files
  focused (<800 lines). Prefer immutability (return new values, don't mutate in
  place). Wrap errors with context (`fmt.Errorf("...: %w", err)`). Never swallow
  errors.
- **UI**: dark mode only. Use the same design tokens as the existing pages
  (see the CSS custom properties in `static/style.css` — never invent colors,
  fonts, or spacing). `JetBrains Mono` for all data/IP/ID/URL/token values,
  status badges for every result, secrets always masked, HTMX attributes for
  dynamic behavior (no bespoke JS), no box-shadows except on modals/dropdowns.
- **No hardcoded values**: magic numbers, thresholds, and timeouts become named
  constants or config.
- **Secrets**: never commit credentials, tokens, or keys — not even in tests or
  example files. Use placeholder values like `test-token`.

## Testing

```bash
go vet ./...
go test -race ./...
go test -cover ./...
```

Tests are table-driven and live next to the code (`*_test.go`). New code should
ship with tests that cover both happy paths and failure modes; keep existing
tests green. There is no CI gate for coverage yet, but aim for meaningful
behavioral coverage rather than line-count gymnastics.

## Git workflow & commits

- Create a feature branch: `git checkout -b feat/duckdns-action` (prefixes:
  `feat/`, `fix/`, `docs/`, `chore/`).
- Keep one logical change per PR. Prefer several small PRs over one large one.
- Conventional Commits, matching the repo history:

  ```
  feat(action): add DuckDNS DDNS action
  fix(cfdns): case-insensitive record lookup
  docs(contributing): document DCO
  ```

  A scope is preferred (`action`, `cfdns`, `mcp`, `ui`, `db`, `ci`, `docs`).

## Opening a pull request

1. **Squash to a small, focused set of commits** on your branch.
2. Fill out the [PR template](.github/PULL_REQUEST_TEMPLATE.md): what changed,
   the motivation, and how you verified it.
3. Reference the issue it closes (`Fixes #19`).
4. Ensure all checks pass: `go vet`, `go test -race ./...`.
5. **Sign off your commits** (see below) — the template's DCO checkbox must be
   checked.
6. Request review from the maintainer. Small PRs get merged fast.

Feedback from review is about the change, not the author — don't take it
personally. If a review asks for a change you disagree with, say so and we'll
find a middle ground.

## License & DCO

wan-ip-check is licensed under the **GNU General Public License v3.0**
([`LICENSE`](LICENSE)). This has two implications for contributors:

1. **Your contribution is GPL-3.0.** By submitting code, you agree it is
   licensed under GPL-3.0 and may be distributed as part of the project.
2. **Only GPL-compatible code may be merged.** Don't copy code from a project
   under a license incompatible with GPL-3.0 (e.g. MIT is fine; proprietary or
   copyleft-incompatible code is not). New dependencies must also be
   GPL-compatible.

To certify you have the right to contribute, each PR must be signed off under
the **Developer Certificate of Origin 1.1**
([text](https://developercertificate.org/)):

```bash
git commit -s    # appends "Signed-off-by: Your Name <you@example.com>"
```

`Signed-off-by` certifies that you wrote the code (or have the right to submit
it) and that it is submitted under GPL-3.0. One sign-off per commit; if a commit
is missing it, amend and re-push. The PR template's DCO checkbox records the same
agreement at the PR level.
