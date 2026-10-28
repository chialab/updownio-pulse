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

### Pre-built binaries

Download the archive for your platform from the [latest release](https://github.com/chialab/updownio-pulse/releases/latest).
For example, on Linux amd64:

```sh
curl -fsSL https://github.com/chialab/updownio-pulse/releases/latest/download/updownio-pulse_linux_amd64.tar.gz \
  | tar -xz -C /usr/local/bin updownio-pulse
```

Checksums are published alongside each release in `checksums.txt`.

### Verifying artifacts

Archives and images come with [build provenance attestations](https://docs.github.com/en/actions/security-for-github-actions/using-artifact-attestations), proving they were built by this repository's release workflow:

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
updownio-pulse --pulse <pulse> [options] -- command [args...]
```

`<pulse>` is either the full URL (`https://pulse.updown.io/xxxx/YYYYYYYY`) or the short form (`xxxx/YYYYYYYY`).

| Option | Default | Description |
|---|---|---|
| `--retry=N` | `5` | Retries on network errors and HTTP 408, 429, 5xx, with exponential backoff. |
| `--timeout=S` | `10` | Per-attempt request timeout, in seconds. |
| `--always` | off | Send the pulse even when the command exits non-zero. |
| `--payload=TPL` | none | [Go template](https://pkg.go.dev/text/template) sent as the request body via POST. Fields: `.ExitCode`. |
| `--version` | | Print version and exit. |

Without `--payload`, the pulse is a plain GET, and it is sent only when the command succeeds.

### Exit codes

`updownio-pulse` always exits with the command's own exit code.
A failed pulse is reported on stderr but never changes it: updown.io will flag the missing pulse anyway.

| Situation | Exit code |
|---|---|
| Command ran | the command's exit code |
| Command killed by a signal | 128 + signal number |
| Command not found | 127 |
| Command could not be started | 126 |
| Invalid options | 2 |

## Examples

### Cron job

```crontab
0 3 * * * updownio-pulse --pulse xxxx/YYYYYYYY -- /opt/scripts/backup.sh
```

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
Note that command-line arguments are visible to other users of the same machine (e.g. via `ps`).

To report a vulnerability, see [SECURITY.md](SECURITY.md).

## Contributing

Contributions are welcome! See [CONTRIBUTING.md](CONTRIBUTING.md) to get started.

## License

[MIT](LICENSE) © Chia Lab s.r.l.
