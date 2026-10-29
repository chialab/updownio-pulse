package main

import (
	"cmp"
	"errors"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// runMainEnv makes the test binary behave as updownio-pulse, so tests can check what reaches stderr.
const runMainEnv = "UPDOWNIO_PULSE_TEST_MAIN"

func TestMain(m *testing.M) {
	if os.Getenv(runMainEnv) == "1" {
		main()
	}

	backoff = func(int) time.Duration { return 0 }

	os.Exit(m.Run())
}

func TestPulseURL(t *testing.T) {
	t.Parallel()

	for input, want := range map[string]string{
		"abcd/EFGH1234":                         "https://pulse.updown.io/abcd/EFGH1234",
		"/abcd/EFGH1234":                        "https://pulse.updown.io/abcd/EFGH1234",
		"https://pulse.updown.io/abcd/EFGH1234": "https://pulse.updown.io/abcd/EFGH1234",
		"http://localhost:8080/abcd/EFGH1234":   "http://localhost:8080/abcd/EFGH1234",
		"":                                      "",
		"ftp://pulse.updown.io/abcd/EFGH1234":   "",
		"https://pulse.updown.io/":              "",
		"https:///abcd":                         "",
	} {
		got, err := pulseURL(input)
		if want == "" {
			if err == nil {
				t.Errorf("pulseURL(%q) = %q, want error", input, got)
			}
		} else if err != nil || got != want {
			t.Errorf("pulseURL(%q) = %q, %v; want %q", input, got, err, want)
		}
	}
}

func TestRedact(t *testing.T) {
	t.Parallel()

	//nolint:gosec // G101: fake credentials, to check they are stripped
	for input, want := range map[string]string{
		"https://pulse.updown.io/abcd/EFGH1234":       "https://pulse.updown.io/abcd/redacted",
		"http://user:pass@localhost:8080/x/y/z?k=v#f": "http://localhost:8080/x/redacted",
		"https:abcd/EFGH1234":                         "redacted",
	} {
		got := redact(input)
		if got != want || strings.Contains(got, "EFGH1234") {
			t.Errorf("redact(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestNewLogger(t *testing.T) {
	t.Parallel()

	var buf strings.Builder

	newLogger(&buf, levelOff).Error("hidden")

	if buf.Len() != 0 {
		t.Errorf("default level logged %q, want nothing", buf.String())
	}

	newLogger(&buf, slog.LevelDebug).Debug("shown")

	if !strings.Contains(buf.String(), "msg=shown") {
		t.Errorf("debug level logged %q, want msg=shown", buf.String())
	}
}

func TestValidate(t *testing.T) {
	t.Parallel()

	valid := func() options {
		return options{pulse: "a/b", retries: 5, timeout: 10, command: []string{"true"}}
	}

	cases := []struct {
		name    string
		mutate  func(o *options)
		wantErr error
	}{
		{"valid", func(*options) {}, nil},
		{"valid payload", func(o *options) { o.payload = "{{.ExitCode}}" }, nil},
		{"missing pulse", func(o *options) { o.pulse = "" }, errMissingPulse},
		{"invalid pulse", func(o *options) { o.pulse = "ftp://x/y" }, errInvalidPulse},
		{"negative retry", func(o *options) { o.retries = -1 }, errInvalidRetry},
		{"zero timeout", func(o *options) { o.timeout = 0 }, errInvalidTimeout},
		{"missing command", func(o *options) { o.command = nil }, errMissingCommand},
		{"bad template", func(o *options) { o.payload = "{{" }, errInvalidPayload},
		{"unknown field", func(o *options) { o.payload = "{{.Nope}}" }, errInvalidPayload},
		{"command index", func(o *options) { o.payload = "{{index .Command 0}}" }, nil},
		{
			"zero time fields", // the dry run leaves them zero: common uses must not fail on it
			func(o *options) {
				o.payload = `{{.StartedAt}} {{.StartedAt.Format "2006-01-02T15:04:05Z07:00"}} ` +
					`{{.FinishedAt.Unix}} {{.Duration}} {{.Duration.Seconds}} {{.Attempt}} {{.ExitCode}}`
			},
			nil,
		},
		{"command index out of range", func(o *options) { o.payload = "{{index .Command 1}}" }, errInvalidPayload},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			opts := valid()
			tc.mutate(&opts)

			err := opts.validate()
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("validate() = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func server(t *testing.T, statuses ...int) (*httptest.Server, *atomic.Int32, *string) {
	t.Helper()

	var (
		hits atomic.Int32
		last string
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(hits.Add(1)) - 1
		b, _ := io.ReadAll(r.Body)
		last = r.Method + " " + string(b)

		w.WriteHeader(statuses[min(n, len(statuses)-1)])
	}))
	t.Cleanup(srv.Close)

	return srv, &hits, &last
}

func TestSendRetries(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		statuses []int
		retries  int
		wantHits int32
		wantErr  bool
	}{
		{"ok", []int{200}, 5, 1, false},
		{"transient then ok", []int{503, 429, 200}, 5, 3, false},
		{"retries exhausted", []int{502}, 2, 3, true},
		{"permanent", []int{404}, 5, 1, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			srv, hits, _ := server(t, tc.statuses...)

			err := send(slog.New(slog.DiscardHandler), srv.URL+"/x/y", nil, tc.retries, time.Second)
			if (err != nil) != tc.wantErr || hits.Load() != tc.wantHits {
				t.Errorf(
					"err=%v hits=%d; want err=%v hits=%d",
					err,
					hits.Load(),
					tc.wantErr,
					tc.wantHits,
				)
			}
		})
	}
}

type runCase struct {
	name     string
	env      map[string]string // on top of UPDOWN_PULSE pointing to the test server
	status   int               // pulse response, 200 if zero
	args     []string          // "$PULSE" is replaced with the test server's pulse URL
	wantCode int
	wantHits int32
	wantLast string
}

// testRun runs each case against a local pulse server.
func testRun(t *testing.T, cases []runCase) {
	t.Helper()

	// Git Bash provides sh on Windows, but without POSIX signal semantics.
	_, err := exec.LookPath("sh")
	if err != nil || runtime.GOOS == "windows" {
		t.Skip("needs a POSIX sh")
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			srv, hits, last := server(t, cmp.Or(tc.status, http.StatusOK))

			pulse := srv.URL + "/x/y"
			env := map[string]string{"UPDOWN_PULSE": pulse}
			maps.Copy(env, tc.env)

			args := make([]string, len(tc.args))
			for i, arg := range tc.args {
				args[i] = strings.ReplaceAll(arg, "$PULSE", pulse)
			}

			code := run(args, func(k string) string { return env[k] })
			if code != tc.wantCode || hits.Load() != tc.wantHits || *last != tc.wantLast {
				t.Errorf(
					"code=%d hits=%d last=%q; want %d %d %q",
					code, hits.Load(), *last, tc.wantCode, tc.wantHits, tc.wantLast,
				)
			}
		})
	}
}

func TestSendRedactsError(t *testing.T) {
	t.Parallel()

	refused, _, _ := server(t, http.StatusOK)
	refused.Close() // connection refused: the error quotes the URL

	// A malformed redirect: the error quotes the Location too.
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "/x/SECRET%")
		w.WriteHeader(http.StatusFound)
	}))
	t.Cleanup(redirect.Close)

	for _, base := range []string{refused.URL, redirect.URL} {
		err := send(slog.New(slog.DiscardHandler), base+"/x/SECRET", nil, 0, time.Second)
		if err == nil || strings.Contains(err.Error(), "SECRET") || !strings.Contains(err.Error(), "/x/redacted") {
			t.Errorf("send() error = %v, want it with the URL redacted", err)
		}
	}
}

func TestRun(t *testing.T) {
	t.Parallel()

	testRun(t, []runCase{
		{"success sends GET", nil, 0, []string{"--", "sh", "-c", "exit 0"}, 0, 1, "GET "},
		{"failure skips pulse", nil, 0, []string{"--", "sh", "-c", "exit 42"}, 42, 0, ""},
		{
			"always with payload", nil, 0,
			[]string{"--always", "--payload=exit-code={{.ExitCode}}", "--", "sh", "-c", "exit 42"},
			42, 1, "POST exit-code=42",
		},
		{
			"short flags", nil, 0,
			[]string{"-a", "-d", "{{.ExitCode}}", "--", "sh", "-c", "exit 3"},
			3, 1, "POST 3",
		},
		{
			"signaled", nil, 0,
			[]string{"--always", "--payload={{.ExitCode}}", "--", "sh", "-c", "kill -TERM $$"},
			143, 1, "POST 143",
		},
		{"not found", nil, 0, []string{"--always", "--", "./does-not-exist"}, 127, 1, "GET "},
		{"usage error", nil, 0, []string{"--timeout=0", "--", "sh", "-c", "exit 0"}, 2, 0, ""},
	})
}

func TestUnknownFlag(t *testing.T) {
	t.Parallel()

	cmd := exec.CommandContext(t.Context(), os.Args[0], "-x") //nolint:gosec // re-runs this test binary as the CLI

	cmd.Env = append(os.Environ(), runMainEnv+"=1")

	var stderr strings.Builder

	cmd.Stderr = &stderr

	err := cmd.Run()

	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != exitUsage {
		t.Fatalf("err = %v, want exit code %d", err, exitUsage)
	}

	want := "updownio-pulse: flag provided but not defined: -x\nUsage: "
	if !strings.HasPrefix(stderr.String(), want) {
		t.Errorf("stderr = %q, want prefix %q", stderr.String(), want)
	}
}

func TestRunEnv(t *testing.T) {
	t.Parallel()

	testRun(t, []runCase{
		{
			"flag overrides invalid env",
			map[string]string{"UPDOWN_PULSE_TIMEOUT": "abc", "UPDOWN_PULSE_RETRIES": "abc"},
			0,
			[]string{"-t", "5", "--retry=1", "--", "sh", "-c", "exit 0"},
			0, 1, "GET ",
		},
		{
			"pulse flag overrides env",
			map[string]string{"UPDOWN_PULSE": "ftp://invalid/x"},
			0,
			[]string{"--pulse", "$PULSE", "--", "sh", "-c", "exit 0"},
			0, 1, "GET ",
		},
		{
			"missing pulse",
			map[string]string{"UPDOWN_PULSE": ""},
			0,
			[]string{"--", "sh", "-c", "exit 0"},
			2, 0, "",
		},
		{
			"invalid env",
			map[string]string{"UPDOWN_PULSE_RETRIES": "abc"},
			0,
			[]string{"--", "sh", "-c", "exit 0"},
			2, 0, "",
		},
		{
			"env validated",
			map[string]string{"UPDOWN_PULSE_TIMEOUT": "0"},
			0,
			[]string{"--", "sh", "-c", "exit 0"},
			2, 0, "",
		},
		{
			"log level",
			map[string]string{"UPDOWN_PULSE_LOG": "debug"},
			0,
			[]string{"--", "sh", "-c", "exit 0"},
			0, 1, "GET ",
		},
		{
			"invalid log level",
			map[string]string{"UPDOWN_PULSE_LOG": "loud"},
			0,
			[]string{"--", "sh", "-c", "exit 0"},
			2, 0, "",
		},
		{
			"flag overrides env",
			map[string]string{"UPDOWN_PULSE_TIMEOUT": "0"},
			0,
			[]string{"-t", "5", "--", "sh", "-c", "exit 0"},
			0, 1, "GET ",
		},
	})
}

func TestPayloadFields(t *testing.T) {
	t.Parallel()

	srv, hits, last := server(t, http.StatusServiceUnavailable, http.StatusOK)

	payload := "{{.ExitCode}} {{.Attempt}} {{.Command}} {{not .StartedAt.IsZero}} " +
		"{{.FinishedAt.Before .StartedAt}} {{ge .Duration.Nanoseconds 0}}"
	cmd := []string{"go", "version"} // a portable command that exists wherever the tests run

	code := run(append([]string{"--payload", payload, "--"}, cmd...), func(k string) string {
		return map[string]string{"UPDOWN_PULSE": srv.URL + "/x/y"}[k]
	})

	want := "POST 0 2 [go version] true false true" // second attempt, after the 503
	if code != 0 || hits.Load() != 2 || *last != want {
		t.Errorf("code=%d hits=%d last=%q; want 0 2 %q", code, hits.Load(), *last, want)
	}
}

func TestRunPulseError(t *testing.T) {
	t.Parallel()

	testRun(t, []runCase{
		{
			"pulse error ignored", nil, http.StatusNotFound,
			[]string{"--", "sh", "-c", "exit 0"},
			0, 1, "GET ",
		},
		{
			"fail on pulse error", nil, http.StatusNotFound,
			[]string{"--fail-on-pulse-error", "--", "sh", "-c", "exit 0"},
			75, 1, "GET ",
		},
		{
			"command code wins over pulse error", nil, http.StatusNotFound,
			[]string{"-f", "-a", "--", "sh", "-c", "exit 3"},
			3, 1, "GET ",
		},
	})
}
