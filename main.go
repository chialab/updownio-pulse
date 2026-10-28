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
	exitCannotExec = 126
	exitNotFound   = 127
	exitSignalBase = 128 // shell convention: 128 + signal number

	maxDrain = 64 << 10 // response bytes read so the connection can be reused
)

var version = "dev"

var (
	errMissingPulse   = errors.New("--pulse is required")
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
	ExitCode int
}

type options struct {
	pulse    string // flag value, normalized to a full URL by validate
	retries  int
	timeout  int
	always   bool
	payload  string
	version  bool
	command  []string
	template *template.Template // parsed payload, nil without --payload
}

func main() { os.Exit(run(os.Args[1:])) }

// warnf reports to stderr; there is nowhere left to report a failed write.
func warnf(format string, a ...any) {
	_, _ = fmt.Fprintf(os.Stderr, "updownio-pulse: "+format+"\n", a...)
}

// newFlagSet binds the command-line flags to opts.
func newFlagSet(opts *options) *flag.FlagSet {
	flags := flag.NewFlagSet("updownio-pulse", flag.ContinueOnError)
	flags.Usage = func() {
		_, _ = fmt.Fprint(flags.Output(), "Usage: updownio-pulse --pulse <pulse> [options] -- command [args...]\n\n")
		_, _ = fmt.Fprint(flags.Output(), "Options:\n")
		flags.PrintDefaults()
	}

	const pulseHelp = "pulse URL (`https://pulse.updown.io/xxxx/YYYYYYYY` or `xxxx/YYYYYYYY`)"
	flags.StringVar(&opts.pulse, "pulse", "", pulseHelp)
	flags.IntVar(&opts.retries, "retry", defaultRetries, "retries on network errors and HTTP 408, 429, 5xx")
	flags.IntVar(&opts.timeout, "timeout", defaultTimeout, "per-attempt request timeout, in seconds")
	flags.BoolVar(&opts.always, "always", false, "send the pulse even when the command exits non-zero")
	flags.StringVar(&opts.payload, "payload", "", "Go template for the request body, sent via POST (fields: .ExitCode)")
	flags.BoolVar(&opts.version, "version", false, "print version and exit")

	return flags
}

func run(args []string) int {
	var opts options

	flags := newFlagSet(&opts)

	err := flags.Parse(args)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}

	if err != nil {
		return exitUsage
	}

	if opts.version {
		_, _ = fmt.Fprintln(os.Stdout, version)

		return 0
	}

	opts.command = flags.Args()

	err = opts.validate()
	if err != nil {
		warnf("%v", err)
		flags.Usage()

		return exitUsage
	}

	code := execute(opts.command)
	if code != 0 && !opts.always {
		return code
	}

	err = notify(&opts, code)
	if err != nil {
		warnf("%v", err)
	}

	return code
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
		err = o.template.Execute(io.Discard, Result{})
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
		warnf("%v", err)

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

// notify renders the payload, if any, and sends the pulse.
func notify(opts *options, code int) error {
	var body []byte

	if opts.template != nil {
		var buf bytes.Buffer

		err := opts.template.Execute(&buf, Result{ExitCode: code})
		if err != nil {
			return fmt.Errorf("payload: %w", err)
		}

		body = buf.Bytes()
	}

	err := send(
		opts.pulse,
		body,
		opts.template != nil,
		opts.retries,
		time.Duration(opts.timeout)*time.Second,
	)
	if err != nil {
		return fmt.Errorf("pulse failed: %w", err)
	}

	return nil
}

// send delivers the pulse, retrying transient failures like `curl --retry`.
func send(target string, body []byte, post bool, retries int, timeout time.Duration) error {
	client := &http.Client{Timeout: timeout}

	for attempt := 0; ; attempt++ {
		retryable, err := sendOnce(client, target, body, post)
		if err == nil || !retryable || attempt >= retries {
			return err
		}

		time.Sleep(backoff(attempt))
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
		return true, fmt.Errorf("request: %w", err)
	}

	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxDrain))
	_ = resp.Body.Close()

	return checkStatus(resp)
}

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
