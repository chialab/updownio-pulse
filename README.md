# updownio-pulse

[![CI](https://github.com/chialab/updownio-pulse/actions/workflows/ci.yml/badge.svg)](https://github.com/chialab/updownio-pulse/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/chialab/updownio-pulse?sort=semver)](https://github.com/chialab/updownio-pulse/releases/latest)
[![Go version](https://img.shields.io/github/go-mod/go-version/chialab/updownio-pulse)](go.mod)
[![License: MIT](https://img.shields.io/github/license/chialab/updownio-pulse)](LICENSE)

**Monitor your cron jobs with [updown.io pulse checks](https://updown.io/doc/how-pulse-cron-monitoring-works), without touching the jobs themselves.**

`updownio-pulse` wraps any command and sends a pulse when it ends:

- 🔌 **Transparent**: stdin, stdout and stderr pass through untouched, signals are forwarded, and the exit code is returned as-is.
- 🔁 **Reliable**: retries transient failures with exponential backoff, like `curl --retry`.
- 🧩 **Expressive**: send a templated body, so updown.io can assert on it (e.g. on the exit code).
- 📦 **Self-contained**: a single static binary for Linux, macOS and Windows, with embedded root CAs for minimal images.

```sh
updownio-pulse --pulse xxxx/YYYYYYYY -- ./backup.sh --full
```

## Installation

### Docker

The image is built `FROM scratch` and ships just the binary, for `linux/amd64`, `linux/arm64` and `linux/arm/v7`.
Copy it into your own image: Docker picks the right architecture automatically.

```dockerfile
COPY --from=ghcr.io/chialab/updownio-pulse:latest /usr/bin/updownio-pulse /usr/bin/updownio-pulse
```

### Linux packages

`.deb`, `.rpm` and `.apk` packages for `amd64`, `arm64` and `armv7` are attached to each release, and install the binary in `/usr/bin`.
For example, on amd64:

```sh
# Debian, Ubuntu
curl -fsSLO https://github.com/chialab/updownio-pulse/releases/latest/download/updownio-pulse_linux_amd64.deb
sudo dpkg -i updownio-pulse_linux_amd64.deb

# Fedora, RHEL
sudo rpm -i https://github.com/chialab/updownio-pulse/releases/latest/download/updownio-pulse_linux_amd64.rpm

# Alpine (packages aren't signed, see "Verifying artifacts" below)
curl -fsSLO https://github.com/chialab/updownio-pulse/releases/latest/download/updownio-pulse_linux_amd64.apk
apk add --allow-untrusted updownio-pulse_linux_amd64.apk
```

### Pre-built binaries

Download the archive for your platform from the [latest release](https://github.com/chialab/updownio-pulse/releases/latest).
For example, on Linux amd64:

```sh
curl -fsSL https://github.com/chialab/updownio-pulse/releases/latest/download/updownio-pulse_linux_amd64.tar.gz \
  | tar -xz -C /usr/local/bin updownio-pulse
```

Checksums are published alongside each release in `checksums.txt`.

### Verifying artifacts

Archives, packages and images come with [build provenance attestations](https://docs.github.com/en/actions/security-for-github-actions/using-artifact-attestations), proving they were built by this repository's release workflow:

```sh
gh attestation verify updownio-pulse_linux_amd64.tar.gz --repo chialab/updownio-pulse
gh attestation verify oci://ghcr.io/chialab/updownio-pulse:latest --repo chialab/updownio-pulse
```

### From source

```sh
go install github.com/chialab/updownio-pulse@latest
```

## Usage

```sh
updownio-pulse [options] -- command [args...]
```

The pulse, given with `--pulse` or `UPDOWN_PULSE`, is either the full URL (`https://pulse.updown.io/xxxx/YYYYYYYY`) or the short form (`xxxx/YYYYYYYY`).

| Option | Environment variable | Default | Description |
|---|---|---|---|
| `-p`, `--pulse=URL` | `UPDOWN_PULSE` | | The pulse to send. Required. |
| `-r`, `--retry=N` | `UPDOWN_PULSE_RETRIES` | `5` | Retries on network errors and HTTP 408, 429, 5xx, with exponential backoff. |
| `-t`, `--timeout=S` | `UPDOWN_PULSE_TIMEOUT` | `10` | Per-attempt request timeout, in seconds. |
| `-a`, `--always` | | off | Send the pulse even when the command exits non-zero. |
| `-d`, `--payload=TPL` | | none | [Go template](https://pkg.go.dev/text/template) sent as the request body via POST. See [payload fields](#payload-fields). |
| `-f`, `--fail-on-pulse-error` | | off | Exit with 75 when the command succeeds but the pulse fails. |
| `-l`, `--log-level=LEVEL` | `UPDOWN_PULSE_LOG` | off | Log to stderr from `debug`, `info`, `warn` or `error` up. See [logging](#logging). |
| `--version` | | | Print version and exit. |

Flags take precedence over environment variables.
Prefer `UPDOWN_PULSE` to `--pulse`: see [Security](#security).

Without `--payload`, the pulse is a plain GET, and it is sent only when the command succeeds.

### Payload fields

| Field | Type | Description |
|---|---|---|
| `.ExitCode` | `int` | The command's exit code, as returned by `updownio-pulse`. |
| `.Attempt` | `int` | 1-based attempt number of the pulse request: greater than 1 after retries. The payload is rendered for each attempt. |
| `.Command` | `[]string` | Command and arguments, as executed. |
| `.StartedAt` | `time.Time` | When the command started, e.g. `{{.StartedAt.Format "2006-01-02T15:04:05Z07:00"}}`. |
| `.FinishedAt` | `time.Time` | When the command exited. |
| `.Duration` | `time.Duration` | How long the command ran, e.g. `1m2.5s`. |

> [!WARNING]
> `.Command` sends the command line to updown.io: don't use it if the arguments contain secrets.

### Exit codes

`updownio-pulse` exits with the command's own exit code.
A failed pulse doesn't change it, because updown.io flags the missing pulse anyway.
To fail right away instead (e.g. in CI), use `--fail-on-pulse-error`.

| Situation | Exit code |
|---|---|
| Command ran | the command's exit code |
| Command killed by a signal | 128 + signal number |
| Command not found | 127 |
| Command could not be started | 126 |
| Command succeeded, pulse failed, with `--fail-on-pulse-error` | 75 |
| Invalid options or environment variables | 2 |

### Logging

By default `updownio-pulse` is silent: only the wrapped command writes to stdout and stderr.
The only exceptions are invalid options and a command that can't be started, which are always reported.

To see what happens, set a log level with `--log-level` or `UPDOWN_PULSE_LOG`.
Each level includes the ones below it:

| Level | What is logged |
|---|---|
| `error` | The pulse failed after all retries. |
| `warn` | A pulse attempt failed and will be retried. |
| `info` | The command's exit code and duration, the pulse sent or skipped. |
| `debug` | The command, the pulse URL (redacted), each attempt and its payload size. |

```sh
$ UPDOWN_PULSE_LOG=info updownio-pulse -- ./backup.sh
time=2026-10-28T03:00:12.345+01:00 level=INFO msg="command exited" exit_code=0 duration=12.3s
time=2026-10-28T03:00:12.512+01:00 level=INFO msg="pulse sent" attempt=1
```

## Examples

### Cron job

```crontab
UPDOWN_PULSE=xxxx/YYYYYYYY
0 3 * * * updownio-pulse -- /opt/scripts/backup.sh
```

The variable applies to every following line, so give each job its own pulse with `--pulse` or a per-line `UPDOWN_PULSE=... updownio-pulse ...` when needed.

### Report failures through the pulse body

By default a failing command sends no pulse, and updown.io notices only when the period expires.
To be alerted right away, always send the pulse with the exit code in the body:

```sh
updownio-pulse --pulse xxxx/YYYYYYYY --always --payload='exit-code={{.ExitCode}}' -- ./backup.sh
```

Then set the check's "contains" field to `exit-code=0`: any other exit code marks the check DOWN.

### Timeouts and overlapping runs

> [!TIP]
> `updownio-pulse` does one thing only, and composes well with standard tools.
>
> Kill a job that hangs with `timeout`: it exits with 124, so the pulse is skipped (or, with `--always` and the [payload above](#report-failures-through-the-pulse-body), reports `exit-code=124`).
>
> ```sh
> updownio-pulse --pulse xxxx/YYYYYYYY -- timeout 1h ./backup.sh
> ```
>
> Skip a run while the previous one is still going with `flock`: no pulse is sent, so updown.io alerts you if it keeps happening.
>
> ```sh
> flock -n /var/lock/backup.lock updownio-pulse --pulse xxxx/YYYYYYYY -- ./backup.sh
> ```
>
> Both come with coreutils/util-linux on Debian and Ubuntu, and with busybox on Alpine, but not with `scratch` or distroless images.

## Security

Treat your pulse URL like a password: anyone who knows it can send pulses and hide real failures.
Command-line arguments are visible to other users of the same machine (e.g. via `ps`).
Pass the URL through `UPDOWN_PULSE` instead, ideally from your secret store.

To report a vulnerability, see [SECURITY.md](SECURITY.md).

## Contributing

Contributions are welcome! See [CONTRIBUTING.md](CONTRIBUTING.md) to get started.

## License

[MIT](LICENSE) © Chia Lab s.r.l.
