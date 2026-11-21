package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/raven-clown/transponder/internal/schema"
)

func TestRetryableStatus(t *testing.T) {
	for status, want := range map[int]bool{
		200: false, 400: false, 404: false, 422: false,
		401: true, 403: true, 408: true, 429: true, 500: true, 503: true, 529: true,
	} {
		if got := RetryableStatus(status); got != want {
			t.Errorf("RetryableStatus(%d) = %v, want %v", status, got, want)
		}
	}
}

func TestErrorMessage(t *testing.T) {
	cause := errors.New("boom")
	err := &Error{Provider: "openai", Status: 503, Err: cause}
	if !strings.Contains(err.Error(), "openai") || !strings.Contains(err.Error(), "503") {
		t.Fatalf("message %q", err.Error())
	}
	if !errors.Is(err, cause) {
		t.Fatal("errors.Is does not reach the cause")
	}
	conn := &Error{Provider: "local", Err: cause}
	if !strings.Contains(conn.Error(), "boom") {
		t.Fatalf("message %q", conn.Error())
	}
}

func TestRetryAfter(t *testing.T) {
	now := time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name   string
		header string
		want   time.Duration
	}{
		{"seconds", "30", 30 * time.Second},
		{"one second", "1", time.Second},
		{"zero", "0", 0},
		{"negative", "-5", 0},
		{"absent", "", 0},
		{"not a number", "soon", 0},
		{"a date", now.Add(45 * time.Second).Format(http.TimeFormat), 45 * time.Second},
		{"a date already past", now.Add(-time.Minute).Format(http.TimeFormat), 0},
		{"over an hour", "7200", MaxRetryAfter},
		{"a number that would overflow", "9223372036854775807", MaxRetryAfter},
		{"a number past int64", "92233720368547758080", MaxRetryAfter},
		{"a negative number past int64", "-92233720368547758080", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := http.Header{}
			if tc.header != "" {
				h.Set("Retry-After", tc.header)
			}
			if got := retryAfter(h, now); got != tc.want {
				t.Fatalf("retryAfter(%q) = %v, want %v", tc.header, got, tc.want)
			}
		})
	}
}

func TestSendKeepsRetryAfter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	_, err := Send(context.Background(), srv.Client(), "p", srv.URL, []byte("{}"), nil, http.Header{})
	var pe *Error
	if !errors.As(err, &pe) || pe.Status != http.StatusTooManyRequests || pe.RetryAfter > 30*time.Second || pe.RetryAfter < 29*time.Second {
		t.Fatalf("err = %v", err)
	}
}

func TestSendCountsDownTheWaitWhileReadingTheBody(t *testing.T) {
	// A wait counts from the headers, so the time the error body takes to arrive comes off it.
	// The body is held open until the test has seen the headers, so the size of the wait owes
	// nothing to how fast the machine is: the answer is the minute the provider asked for, less
	// however long the body was held.
	started, release := make(chan struct{}), make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusTooManyRequests)
		w.(http.Flusher).Flush()
		close(started)
		<-release
		_, _ = w.Write([]byte("{}"))
	}))
	defer srv.Close()
	done := make(chan error, 1)
	go func() {
		_, err := Send(context.Background(), srv.Client(), "p", srv.URL, []byte("{}"), nil, http.Header{})
		done <- err
	}()
	<-started
	close(release)
	err := <-done
	var pe *Error
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v", err)
	}
	if pe.RetryAfter <= 0 || pe.RetryAfter >= time.Minute {
		t.Fatalf("RetryAfter = %v, want the minute less the time the body took", pe.RetryAfter)
	}
}

func TestHeaderContext(t *testing.T) {
	if _, ok := HeaderFrom(context.Background()); ok {
		t.Fatal("bare context has headers")
	}
	h := http.Header{"X-Request-Id": {"r1"}}
	got, ok := HeaderFrom(WithHeader(context.Background(), h))
	if !ok || got.Get("X-Request-ID") != "r1" {
		t.Fatalf("headers %v", got)
	}
}

func TestCannotCarry(t *testing.T) {
	plain := errors.New("boom")
	if got := CannotCarry(plain); got != plain { //nolint:errorlint // the same value must come back, not a wrap of it
		t.Fatalf("a plain error came back as %v", got)
	}
	if CannotCarry(nil) != nil {
		t.Fatal("nil came back as an error")
	}
	err := CannotCarry(fmt.Errorf("encoding: %w", &schema.ContentError{Reason: "no images at a URL"}))
	var ce *schema.ContentError
	if !errors.Is(err, ErrUnsupported) || !errors.As(err, &ce) || ce.Reason != "no images at a URL" {
		t.Fatalf("err = %v", err)
	}
}
