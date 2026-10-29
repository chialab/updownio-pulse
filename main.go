// Command updownio-pulse wraps a command and sends an updown.io pulse when it ends.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"text/template"
	"time"

	// Embedded root CAs, used only when the system has none (e.g. scratch images).
	_ "golang.org/x/crypto/x509roots/fallback"
)

const (
	pulseBase = "https://pulse.updown.io/"

	defaultRetries = 5
	defaultTimeout = 10 // seconds
	maxBackoffExp  = 5  // caps the wait between retries at 32s

	exitUsage      = 2
	exitTempFail   = 75 // EX_TEMPFAIL from sysexits.h
	exitCannotExec = 126
	exitNotFound   = 127
	exitSignalBase = 128 // shell convention: 128 + signal number

	maxDrain = 64 << 10 // response bytes read so the connection can be reused

	levelOff = slog.LevelError + 1 // above every level logged: silent by default
)

var version = "dev"

var (
	errMissingPulse   = errors.New("--pulse or UPDOWN_PULSE is required")
	errInvalidEnv     = errors.New("invalid")
	errInvalidPulse   = errors.New("invalid --pulse")
	errInvalidRetry   = errors.New("--retry must be >= 0")
	errInvalidTimeout = errors.New("--timeout must be > 0")
	errMissingCommand = errors.New("missing command")
	errInvalidPayload = errors.New("invalid --payload")
	errHTTPStatus     = errors.New("unexpected HTTP status")
)

// backoff is the wait before retry number i (0-based).
//
//nolint:gochecknoglobals // overridden in tests to keep them fast
var backoff = func(i int) time.Duration { return time.Second << min(i, maxBackoffExp) }

// Result is the data available to the --payload template.
type Result struct {
	ExitCode   int
	Attempt    int      // 1-based pulse request attempt, the template is rendered for each
	Command    []string // command and arguments, as executed
	StartedAt  time.Time
	FinishedAt time.Time
	Duration   time.Duration
}

type options struct {
	pulse       string // flag value, normalized to a full URL by validate
	retries     int
	timeout     int
	always      bool
	payload     string
	failOnPulse bool
	logLevel    slog.Level
	version     bool
	command     []string
	template    *template.Template // parsed payload, nil without --payload
	log         *slog.Logger
}

// envVars maps flags to the environment variables that set their value when the flag is not given.
//
//nolint:gochecknoglobals // read-only table
var envVars = []struct{ flag, name string }{
	{"pulse", "UPDOWN_PULSE"},
	{"retry", "UPDOWN_PULSE_RETRIES"},
	{"timeout", "UPDOWN_PULSE_TIMEOUT"},
	{"log-level", "UPDOWN_PULSE_LOG"},
}

const usage = `Usage: updownio-pulse [options] -- command [args...]

Options:
  -p, --pulse <pulse>        pulse URL (https://pulse.updown.io/xxxx/YYYYYYYY or xxxx/YYYYYYYY) [$UPDOWN_PULSE]
  -r, --retry <n>            retries on network errors and HTTP 408, 429, 5xx (default %d) [$UPDOWN_PULSE_RETRIES]
  -t, --timeout <seconds>    per-attempt request timeout (default %d) [$UPDOWN_PULSE_TIMEOUT]
  -a, --always               send the pulse even when the command exits non-zero
  -d, --payload <template>   Go template for the request body, sent via POST
                             (fields: .ExitCode, .Attempt, .Command, .StartedAt, .FinishedAt, .Duration)
  -f, --fail-on-pulse-error  exit with %d when the command succeeds but the pulse fails
  -l, --log-level <level>    log to stderr from debug, info, warn or error up (default: off) [$UPDOWN_PULSE_LOG]
      --version              print version and exit
  -h, --help                 print this help and exit

Flags take precedence over environment variables.
`

func main() { os.Exit(run(os.Args[1:], os.Getenv)) }

// newLogger logs to w from level up, as slog text. Errors that must always show use warn instead.
func newLogger(w io.Writer, level slog.Level) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: level}))
}

// redact hides the pulse URL's secret part, keeping its first path segment to tell pulses apart.
func redact(pulse string) string {
	parsed, err := url.Parse(pulse)
	if err != nil || parsed.Opaque != "" { // String() prints Opaque, not Path, which would leak
		return "redacted"
	}

	first, _, _ := strings.Cut(strings.TrimPrefix(parsed.Path, "/"), "/")
	parsed.User, parsed.Path, parsed.RawQuery, parsed.Fragment = nil, "/"+first+"/redacted", "", ""

	return parsed.String()
}

// warn reports to stderr, whatever the log level; there is nowhere left to report a failed write.
func warn(err error) {
	_, _ = fmt.Fprintf(os.Stderr, "updownio-pulse: %v\n", err)
}

// printUsage writes the help text to stderr.
func printUsage() {
	_, _ = fmt.Fprintf(os.Stderr, usage, defaultRetries, defaultTimeout, exitTempFail)
}

// newFlagSet binds the command-line flags to opts. Help texts live in usage, which also lists the short aliases.
func newFlagSet(opts *options) *flag.FlagSet {
	flags := flag.NewFlagSet("updownio-pulse", flag.ContinueOnError)
	// Parse errors are reported by run through warn, with the usual prefix.
	flags.SetOutput(io.Discard)

	flags.StringVar(&opts.pulse, "pulse", "", "")
	flags.IntVar(&opts.retries, "retry", defaultRetries, "")
	flags.IntVar(&opts.timeout, "timeout", defaultTimeout, "")
	flags.BoolVar(&opts.always, "always", false, "")
	flags.StringVar(&opts.payload, "payload", "", "")
	flags.BoolVar(&opts.failOnPulse, "fail-on-pulse-error", false, "")
	flags.TextVar(&opts.logLevel, "log-level", levelOff, "")
	flags.BoolVar(&opts.version, "version", false, "")

	for short, long := range map[string]string{
		"p": "pulse", "r": "retry", "t": "timeout", "a": "always", "d": "payload", "f": "fail-on-pulse-error",
		"l": "log-level",
	} {
		flags.Var(flags.Lookup(long).Value, short, "")
	}

	return flags
}

// applyEnv sets the flags not given on the command line from their environment variables.
func applyEnv(flags *flag.FlagSet, getenv func(string) string) error {
	// Short aliases share the long flag's Value, so this catches both spellings.
	given := map[flag.Value]bool{}

	flags.Visit(func(f *flag.Flag) { given[f.Value] = true })

	for _, env := range envVars {
		value := getenv(env.name)
		if value == "" || given[flags.Lookup(env.flag).Value] {
			continue
		}

		err := flags.Set(env.flag, value)
		if err != nil {
			return fmt.Errorf("%w %s: %w", errInvalidEnv, env.name, err)
		}
	}

	return nil
}

func run(args []string, getenv func(string) string) int {
	var opts options

	flags := newFlagSet(&opts)

	err := flags.Parse(args)
	if errors.Is(err, flag.ErrHelp) {
		printUsage()

		return 0
	}

	if err != nil {
		warn(err)
		printUsage()

		return exitUsage
	}

	if opts.version {
		_, _ = fmt.Fprintln(os.Stdout, version)

		return 0
	}

	opts.command = flags.Args()

	err = applyEnv(flags, getenv)
	if err == nil {
		err = opts.validate()
	}

	if err != nil {
		warn(err)
		printUsage()

		return exitUsage
	}

	opts.log = newLogger(os.Stderr, opts.logLevel)
	opts.log.Debug("running command", "command", opts.command)

	started := time.Now()
	code := execute(opts.command)
	finished := time.Now()

	opts.log.Info("command exited", "exit_code", code, "duration", finished.Sub(started))

	return finish(&opts, Result{
		ExitCode:   code,
		Command:    opts.command,
		StartedAt:  started.Round(0), // drops the monotonic reading, which would show up when printed
		FinishedAt: finished.Round(0),
		Duration:   finished.Sub(started),
	})
}

// finish sends the pulse, if due, after the command exited, and returns the wrapper's exit code.
func finish(opts *options, result Result) int {
	if result.ExitCode != 0 && !opts.always {
		opts.log.Info("pulse skipped: the command failed, see --always")

		return result.ExitCode
	}

	err := notify(opts, result)
	if err != nil {
		opts.log.Error("pulse failed", "error", err)

		if opts.failOnPulse && result.ExitCode == 0 {
			return exitTempFail
		}
	}

	return result.ExitCode
}

// validate checks the options before the command runs, so mistakes fail fast.
func (o *options) validate() error {
	var err error

	o.pulse, err = pulseURL(o.pulse)
	if err != nil {
		return err
	}

	switch {
	case o.retries < 0:
		return errInvalidRetry
	case o.timeout <= 0:
		return errInvalidTimeout
	case len(o.command) == 0:
		return errMissingCommand
	case o.payload == "":
		return nil
	}

	o.template, err = template.New("payload").Parse(o.payload)
	if err == nil {
		// Dry run so unknown fields fail now, not after the command ran.
		err = o.template.Execute(io.Discard, Result{Attempt: 1, Command: o.command})
	}

	if err != nil {
		return fmt.Errorf("%w: %w", errInvalidPayload, err)
	}

	return nil
}

// pulseURL accepts either a full pulse URL or the short `xxxx/YYYYYYYY` form.
func pulseURL(raw string) (string, error) {
	if raw == "" {
		return "", errMissingPulse
	}

	if !strings.Contains(raw, "://") {
		raw = pulseBase + strings.TrimPrefix(raw, "/")
	}

	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" ||
		strings.Trim(parsed.Path, "/") == "" {
		return "", fmt.Errorf("%w %q", errInvalidPulse, raw)
	}

	return parsed.String(), nil
}

// execute runs the command with inherited stdio, forwarding signals, and returns its exit code.
func execute(command []string) int {
	//nolint:gosec // G204: running the user's command is the point
	cmd := exec.CommandContext(context.Background(), command[0], command[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, forwardedSignals()...)

	defer signal.Stop(sigs)

	err := cmd.Start()
	if err != nil {
		warn(err)

		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
			return exitNotFound
		}

		return exitCannotExec
	}

	go func() {
		for s := range sigs {
			_ = cmd.Process.Signal(
				s,
			) // best effort: fails on Windows, where the console already delivers Ctrl+C to the child
		}
	}()

	_ = cmd.Wait() // a non-zero exit is reported via ProcessState

	code := cmd.ProcessState.ExitCode()
	if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		code = exitSignalBase + int(ws.Signal())
	}

	return code
}

// notify sends the pulse, with the payload, if any, rendered for each attempt.
func notify(opts *options, result Result) error {
	var render func(attempt int) ([]byte, error)

	if opts.template != nil {
		render = func(attempt int) ([]byte, error) {
			var buf bytes.Buffer

			result.Attempt = attempt

			err := opts.template.Execute(&buf, result)
			if err != nil {
				return nil, fmt.Errorf("payload: %w", err)
			}

			return buf.Bytes(), nil
		}
	}

	opts.log.Debug("sending pulse", "pulse", redact(opts.pulse), "post", render != nil)

	return send(opts.log, opts.pulse, render, opts.retries, time.Duration(opts.timeout)*time.Second)
}

// send delivers the pulse, retrying transient failures like `curl --retry`.
// With render, the pulse is a POST with the body it returns for each 1-based attempt; without, a GET.
func send(
	log *slog.Logger,
	target string,
	render func(attempt int) ([]byte, error),
	retries int,
	timeout time.Duration,
) error {
	client := &http.Client{Timeout: timeout}

	for attempt := 1; ; attempt++ {
		var body []byte

		if render != nil {
			var err error

			body, err = render(attempt)
			if err != nil {
				return err
			}
		}

		log.Debug("pulse attempt", "attempt", attempt, "body_bytes", len(body))

		retryable, err := sendOnce(client, target, body, render != nil)
		if err == nil {
			log.Info("pulse sent", "attempt", attempt)

			return nil
		}

		if !retryable || attempt > retries {
			return err
		}

		wait := backoff(attempt - 1)
		log.Warn("pulse attempt failed, retrying", "attempt", attempt, "error", err, "backoff", wait)
		time.Sleep(wait)
	}
}

func sendOnce(client *http.Client, target string, body []byte, post bool) (bool, error) {
	method, reader := http.MethodGet, io.Reader(nil)
	if post {
		method, reader = http.MethodPost, bytes.NewReader(body)
	}

	req, err := http.NewRequestWithContext(context.Background(), method, target, reader)
	if err != nil {
		return false, fmt.Errorf("request: %w", err)
	}

	req.Header.Set("User-Agent", "updownio-pulse/"+version)

	if post {
		req.Header.Set("Content-Type", "text/plain; charset=utf-8")
	}

	resp, err := client.Do(req)
	if err != nil {
		// The error quotes the URL, and redirect ones too: hide the secret part, as it ends up in the logs.
		_, secret, _ := strings.Cut(strings.TrimPrefix(req.URL.EscapedPath(), "/"), "/")

		return true, redactedError{fmt.Errorf("request: %w", err), secret}
	}

	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxDrain))
	_ = resp.Body.Close()

	return checkStatus(resp)
}

// redactedError hides secret from the message of the error it wraps.
type redactedError struct {
	err    error
	secret string
}

func (e redactedError) Error() string {
	if e.secret == "" {
		return e.err.Error()
	}

	return strings.ReplaceAll(e.err.Error(), e.secret, "redacted")
}

func (e redactedError) Unwrap() error { return e.err }

// checkStatus reports whether the response is an error, and whether that error is worth retrying.
func checkStatus(resp *http.Response) (bool, error) {
	switch c := resp.StatusCode; {
	case c >= http.StatusOK && c < http.StatusMultipleChoices:
		return false, nil
	case c == http.StatusRequestTimeout || c == http.StatusTooManyRequests || c >= http.StatusInternalServerError:
		return true, fmt.Errorf("%w: %s", errHTTPStatus, resp.Status)
	default:
		return false, fmt.Errorf("%w: %s", errHTTPStatus, resp.Status)
	}
}
