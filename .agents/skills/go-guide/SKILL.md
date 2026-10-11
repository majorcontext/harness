---
name: go-guide
description: Go code and API rules to follow when writing or reviewing Go. Covers module and package layout, naming, API design, errors, concurrency, testing, and comments. Use before adding a package, an exported API, or tests.
---

# Go guide

One rule per line, with its source where Go documents it. If the repo's AGENTS.md or CLAUDE.md differs, the repo wins.

## Layout

- Put commands in `cmd/` and private packages in `internal/`; keep `cmd/` to flags and wiring. [go.dev/doc/modules/layout](https://go.dev/doc/modules/layout)
- Give each package one purpose, named for what it provides. [go.dev/blog/package-names](https://go.dev/blog/package-names)
- Do not name a package `util`, `common`, `misc`, `helpers`, `api`, `types`, or `interfaces`. [go.dev/wiki/CodeReviewComments#package-names](https://go.dev/wiki/CodeReviewComments#package-names)
- Group files by concern, not by kind (no `types.go` or `interfaces.go` that collect everything).
- A large file or function needs a reason; split by concern before it grows.

## Naming

- Follow Effective Go and Code Review Comments for names, initialisms, and receivers. [go.dev/doc/effective_go#names](https://go.dev/doc/effective_go#names), [go.dev/wiki/CodeReviewComments](https://go.dev/wiki/CodeReviewComments)
- Do not stutter: `session.Store`, not `session.SessionStore`. [go.dev/blog/package-names](https://go.dev/blog/package-names)

## API design

- Take `context.Context` first and never store it in a struct. [pkg.go.dev/context](https://pkg.go.dev/context), [go.dev/wiki/CodeReviewComments#contexts](https://go.dev/wiki/CodeReviewComments#contexts)
- Return concrete types. Declare small interfaces in the consumer, not the producer, and not before a use exists. [go.dev/wiki/CodeReviewComments#interfaces](https://go.dev/wiki/CodeReviewComments#interfaces)
- Make the zero value useful, or provide a constructor that validates. [go.dev/doc/effective_go#allocation_new](https://go.dev/doc/effective_go#allocation_new)
- Configure with one options struct whose zero values are documented defaults.
- Document concurrency safety, ownership, and `Close` semantics (idempotent or not, what it waits for). [go.dev/doc/comment#type](https://go.dev/doc/comment#type)
- Treat errors as values: export sentinels or typed errors, match with `errors.Is` and `errors.As`, never by string. [go.dev/blog/go1.13-errors](https://go.dev/blog/go1.13-errors)
- Wrap with `%w` when the caller may match the cause; otherwise do not expose it. [go.dev/blog/go1.13-errors#whether-to-wrap](https://go.dev/blog/go1.13-errors#whether-to-wrap)
- Return errors; do not panic for normal error handling. [go.dev/wiki/CodeReviewComments#dont-panic](https://go.dev/wiki/CodeReviewComments#dont-panic)
- Return streams as `iter.Seq` or `iter.Seq2`. [pkg.go.dev/iter](https://pkg.go.dev/iter)
- Keep no global state: inject loggers (`*slog.Logger`) and environment readers.
- Report API breaks with apidiff before release. [pkg.go.dev/golang.org/x/exp/cmd/apidiff](https://pkg.go.dev/golang.org/x/exp/cmd/apidiff)

## Concurrency

- Share memory by communicating. [go.dev/doc/effective_go#sharing](https://go.dev/doc/effective_go#sharing)
- One goroutine owns each piece of data; others get copies or views.
- Every goroutine has an owner that starts it and a way to stop it. [go.dev/wiki/CodeReviewComments#goroutine-lifetimes](https://go.dev/wiki/CodeReviewComments#goroutine-lifetimes)
- Prefer synchronous functions; the caller adds concurrency. [go.dev/wiki/CodeReviewComments#synchronous-functions](https://go.dev/wiki/CodeReviewComments#synchronous-functions)

## Testing

- Write contract tests through the public entry point; test internals only for a stated reason (a race no contract test reaches, a codec with many edge cases).
- Use table rows for variants. [go.dev/wiki/TableDrivenTests](https://go.dev/wiki/TableDrivenTests)
- Use `testing/synctest` for time; do not sleep. [pkg.go.dev/testing/synctest](https://pkg.go.dev/testing/synctest)
- Write repo tooling in Go (`go run ./internal/<tool>` or a test), not shell scripts.

## Comments

- Write no history in comments (no PR numbers, dates, or "changed from"); commit messages hold history.
- Say what the code does and why it must; do not restate the code. [go.dev/doc/comment](https://go.dev/doc/comment)
