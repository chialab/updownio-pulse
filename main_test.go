package main

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
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

			err := send(srv.URL+"/x/y", nil, false, tc.retries, time.Second)
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

func TestRun(t *testing.T) {
	t.Parallel()

	// Git Bash provides sh on Windows, but without POSIX signal semantics.
	_, err := exec.LookPath("sh")
	if err != nil || runtime.GOOS == "windows" {
		t.Skip("needs a POSIX sh")
	}

	cases := []struct {
		name     string
		args     []string
		wantCode int
		wantHits int32
		wantLast string
	}{
		{"success sends GET", []string{"--", "sh", "-c", "exit 0"}, 0, 1, "GET "},
		{"failure skips pulse", []string{"--", "sh", "-c", "exit 42"}, 42, 0, ""},
		{
			"always with payload",
			[]string{"--always", "--payload=exit-code={{.ExitCode}}", "--", "sh", "-c", "exit 42"},
			42, 1, "POST exit-code=42",
		},
		{
			"signaled",
			[]string{"--always", "--payload={{.ExitCode}}", "--", "sh", "-c", "kill -TERM $$"},
			143, 1, "POST 143",
		},
		{"not found", []string{"--always", "--", "./does-not-exist"}, 127, 1, "GET "},
		{"usage error", []string{"--timeout=0", "--", "sh", "-c", "exit 0"}, 2, 0, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			srv, hits, last := server(t, 200)

			code := run(append([]string{"--pulse", srv.URL + "/x/y"}, tc.args...))
			if code != tc.wantCode || hits.Load() != tc.wantHits || *last != tc.wantLast {
				t.Errorf(
					"code=%d hits=%d last=%q; want %d %d %q",
					code, hits.Load(), *last, tc.wantCode, tc.wantHits, tc.wantLast,
				)
			}
		})
	}
}
