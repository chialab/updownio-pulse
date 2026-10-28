# Agent instructions

`updownio-pulse` is a single-binary Go CLI: it runs a command with transparent stdio, forwards signals, returns the command's exit code, and then sends an [updown.io pulse](https://updown.io/doc/how-pulse-cron-monitoring-works).
User-facing docs live in [README.md](README.md); contribution rules in [CONTRIBUTING.md](CONTRIBUTING.md).

## Layout

- `main.go`: everything (flags, validation, command execution, HTTP pulse with retries).
- `signals_unix.go`, `signals_windows.go`: the per-platform list of forwarded signals.
- `main_test.go`: table-driven tests, all `t.Parallel()`.
- `.goreleaser.yaml` + `Dockerfile`: release of binaries (linux/darwin/windows, amd64/arm64, linux/arm/v7) and the multi-arch `FROM scratch` image on GHCR.
- `.github/workflows/`: `ci.yml` (test matrix with `-race`, golangci-lint, govulncheck, zizmor) and `release.yml` (GoReleaser on `v*` tags).

## Commands

Run all of these before declaring a change done:

```sh
go test -race ./...
golangci-lint fmt && golangci-lint run
GOOS=windows golangci-lint run      # Windows-only files are linted separately
go tool govulncheck ./...
go mod tidy -diff
zizmor --persona=pedantic .         # when touching .github/
goreleaser check                    # when touching .goreleaser.yaml
```

golangci-lint and zizmor may be missing locally (e.g. in the dev container): CI runs them, so say so instead of installing them ad hoc.
Tests start a local HTTP server, so they need local port binding.

## Behavior contract

These are deliberate decisions; don't change them without being asked:

- The wrapper's exit code is the command's: 128 + signal if killed, 127 not found, 126 not startable, 2 for invalid options or environment variables.
- A failed pulse doesn't change the exit code, except with `--fail-on-pulse-error`: then 75 (`EX_TEMPFAIL`), only if the command succeeded.
- Silent by default: diagnostics go through `slog` (text handler, stderr) only with `--log-level`/`UPDOWN_PULSE_LOG`. Usage errors and a command that can't be started are always printed, plain, via `warn`. Never log the pulse URL unredacted.
- `UPDOWN_PULSE`, `UPDOWN_PULSE_RETRIES`, `UPDOWN_PULSE_TIMEOUT` set `--pulse`, `--retry`, `--timeout`; precedence is flag > env > default. Other flags have no variable on purpose.
- Flags are parsed with the stdlib `flag` package; short aliases share the long flag's `Value`, and the help text is the hand-written `usage` constant (keep it in sync).
- Without `--always`, no pulse is sent when the command fails.
- Without `--payload` the pulse is a GET; with it, a POST with the template rendered for each attempt (`.Attempt` changes) as body. Template fields are the exported fields of `Result`; keep README's payload fields table and `usage` in sync.
- `--retry=N` means N retries after the first attempt (like `curl --retry`), only on network errors and HTTP 408, 429, 5xx.
- Options, including the payload template, are validated before running the command.
- No buffering of stdout/stderr: output streams through untouched.

## Conventions

- **Linting is maximal**: `.golangci.yml` enables every linter except a few documented ones. Fix findings rather than disabling linters. For a genuine false positive use `//nolint:<linter> // <reason>`: the reason is mandatory and `nolintlint` checks it.
- **No new dependencies** unless asked: the `depguard` allowlist in `.golangci.yml` only permits the stdlib and `golang.org/x/crypto/x509roots/fallback`.
- Errors are sentinel values wrapped with `%w`, tested with `errors.Is`.
- Named constants instead of magic numbers; plain `err :=` assignments, not inline `if err := ...`.
- GitHub Actions are referenced by **major tag, not SHA** (on purpose, see `.github/zizmor.yml`); keep least-privilege `permissions` and `persist-credentials: false`.
- Keep README and this file in sync when flags or behavior change.
- User-facing text, code, comments and commits are in English.
