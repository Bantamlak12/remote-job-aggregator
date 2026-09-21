// These tests live in the package rather than in httpclient_test because
// two of the things worth proving are not reachable from outside it: the
// backoff calculation (a pure function, tested without a clock) and the
// transport wiring (a configuration decision with no observable runtime
// behavior in a single-request test). Everything else here goes through
// the exported API exactly as a caller would.
//
// No test in this file touches a real network: every request goes to an
// httptest server on loopback, or to a port deliberately left dead.
package httpclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// testConfig is DefaultConfig with the timeout tightened, so a hung test
// fails in seconds instead of stalling the suite.
func testConfig() Config {
	cfg := DefaultConfig()
	cfg.Timeout = 5 * time.Second
	return cfg
}

func newRequest(t *testing.T, ctx context.Context, url string) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("http.NewRequestWithContext() failed: %v", err)
	}
	return req
}

// unreachableAddr returns a host:port with nothing listening on it: bind
// an ephemeral port, then release it. A dial there is refused
// immediately, which keeps the network-error tests fast and independent
// of how this machine's network treats unroutable addresses.
func unreachableAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() failed: %v", err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatalf("closing probe listener failed: %v", err)
	}
	return addr
}

// doWithin runs Do in a goroutine and fails the test if it has not
// returned within limit. Several tests below exist precisely because a
// bug would make Do wait too long; without this bound, that bug would
// hang the suite instead of failing it.
func doWithin(t *testing.T, limit time.Duration, c *Client, req *http.Request) (*http.Response, error) {
	t.Helper()

	type result struct {
		resp *http.Response
		err  error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := c.Do(req)
		done <- result{resp, err}
	}()

	select {
	case r := <-done:
		return r.resp, r.err
	case <-time.After(limit):
		t.Fatalf("Do() did not return within %s", limit)
		return nil, nil
	}
}

func TestDefaultConfig_Values(t *testing.T) {
	cfg := DefaultConfig()

	tests := []struct {
		name string
		got  any
		want any
	}{
		{"Timeout", cfg.Timeout, 10 * time.Second},
		{"MaxResponseBytes", cfg.MaxResponseBytes, int64(5 * 1024 * 1024)},
		{"UserAgent", cfg.UserAgent, "remote-job-aggregator/1.0"},
		{"MaxIdleConns", cfg.MaxIdleConns, 100},
		{"MaxIdleConnsPerHost", cfg.MaxIdleConnsPerHost, 10},
		{"IdleConnTimeout", cfg.IdleConnTimeout, 90 * time.Second},
		{"MaxRetries", cfg.MaxRetries, 3},
		{"MaxRedirects", cfg.MaxRedirects, 5},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("DefaultConfig().%s = %v, want %v", tt.name, tt.got, tt.want)
		}
	}
}

func TestNew_ConfiguresTransportFromConfig(t *testing.T) {
	cfg := Config{
		Timeout:             3 * time.Second,
		MaxIdleConns:        42,
		MaxIdleConnsPerHost: 7,
		IdleConnTimeout:     11 * time.Second,
	}
	c := New(cfg)

	if c.httpClient.Timeout != cfg.Timeout {
		t.Errorf("http.Client.Timeout = %s, want %s", c.httpClient.Timeout, cfg.Timeout)
	}
	if c.httpClient.CheckRedirect == nil {
		t.Error("http.Client.CheckRedirect is nil; redirect chains would be capped by net/http's default of 10, not by Config.MaxRedirects")
	}

	transport, ok := c.httpClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport is %T, want *http.Transport", c.httpClient.Transport)
	}
	if transport == http.DefaultTransport {
		t.Fatal("Transport is http.DefaultTransport; pool tuning must not leak into every other net/http user in the process")
	}
	if transport.MaxIdleConns != cfg.MaxIdleConns {
		t.Errorf("Transport.MaxIdleConns = %d, want %d", transport.MaxIdleConns, cfg.MaxIdleConns)
	}
	if transport.MaxIdleConnsPerHost != cfg.MaxIdleConnsPerHost {
		t.Errorf("Transport.MaxIdleConnsPerHost = %d, want %d", transport.MaxIdleConnsPerHost, cfg.MaxIdleConnsPerHost)
	}
	if transport.IdleConnTimeout != cfg.IdleConnTimeout {
		t.Errorf("Transport.IdleConnTimeout = %s, want %s", transport.IdleConnTimeout, cfg.IdleConnTimeout)
	}
	if transport.TLSClientConfig != nil {
		t.Errorf("Transport.TLSClientConfig = %+v, want nil so Go's verified TLS defaults stay in place", transport.TLSClientConfig)
	}
}

func TestDo_SetsUserAgentWhenAbsent(t *testing.T) {
	var got atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.Store(r.Header.Get("User-Agent"))
	}))
	defer srv.Close()

	cfg := testConfig()
	cfg.UserAgent = "test-agent/9.9"
	resp, err := New(cfg).Do(newRequest(t, context.Background(), srv.URL))
	if err != nil {
		t.Fatalf("Do() failed: %v", err)
	}
	defer resp.Body.Close()

	if got.Load() != cfg.UserAgent {
		t.Errorf("server saw User-Agent %q, want %q", got.Load(), cfg.UserAgent)
	}
}

func TestDo_PreservesCallerUserAgent(t *testing.T) {
	var got atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.Store(r.Header.Get("User-Agent"))
	}))
	defer srv.Close()

	req := newRequest(t, context.Background(), srv.URL)
	req.Header.Set("User-Agent", "caller/1.2")

	resp, err := New(testConfig()).Do(req)
	if err != nil {
		t.Fatalf("Do() failed: %v", err)
	}
	defer resp.Body.Close()

	if got.Load() != "caller/1.2" {
		t.Errorf("server saw User-Agent %q, want the caller's %q", got.Load(), "caller/1.2")
	}
}

// TestDo_EmptyUserAgentSendsNoUserAgentHeader pins down what a Config
// with no UserAgent actually puts on the wire. net/http treats a header
// that is present but empty as "omit this header", so the result is no
// User-Agent at all rather than Go's default "Go-http-client/1.1" — worth
// a test rather than a claim, since the two are indistinguishable from
// the calling code.
func TestDo_EmptyUserAgentSendsNoUserAgentHeader(t *testing.T) {
	var got atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.Store(r.Header.Get("User-Agent"))
	}))
	defer srv.Close()

	cfg := testConfig()
	cfg.UserAgent = ""
	resp, err := New(cfg).Do(newRequest(t, context.Background(), srv.URL))
	if err != nil {
		t.Fatalf("Do() failed: %v", err)
	}
	defer resp.Body.Close()

	if got.Load() != "" {
		t.Errorf("server saw User-Agent %q, want none", got.Load())
	}
}

// TestDo_ReusesPooledConnections is the test for the reason this package
// exists. It also guards the body wrapper: a wrapper that swallowed the
// underlying io.EOF would leave the transport unable to tell the response
// had ended, and every request would silently open a fresh connection.
func TestDo_ReusesPooledConnections(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.WriteString(w, "body"); err != nil {
			t.Errorf("writing test body failed: %v", err)
		}
	}))
	defer srv.Close()

	c := New(testConfig())

	first, err := c.Do(newRequest(t, context.Background(), srv.URL))
	if err != nil {
		t.Fatalf("first Do() failed: %v", err)
	}
	if _, err := io.ReadAll(first.Body); err != nil {
		t.Fatalf("draining the first body failed: %v", err)
	}
	if err := first.Body.Close(); err != nil {
		t.Fatalf("closing the first body failed: %v", err)
	}

	var reused atomic.Bool
	ctx := httptrace.WithClientTrace(context.Background(), &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) { reused.Store(info.Reused) },
	})
	second, err := c.Do(newRequest(t, ctx, srv.URL))
	if err != nil {
		t.Fatalf("second Do() failed: %v", err)
	}
	defer second.Body.Close()

	if !reused.Load() {
		t.Error("the second request opened a new connection; the idle pool is not being used, which is the whole point of sharing one Client")
	}
}

func TestDo_BodyExactlyAtLimitReadsNormally(t *testing.T) {
	const limit = 1024
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := w.Write(make([]byte, limit)); err != nil {
			t.Errorf("writing test body failed: %v", err)
		}
	}))
	defer srv.Close()

	cfg := testConfig()
	cfg.MaxResponseBytes = limit
	resp, err := New(cfg).Do(newRequest(t, context.Background(), srv.URL))
	if err != nil {
		t.Fatalf("Do() failed: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("io.ReadAll() on a body of exactly MaxResponseBytes returned %v, want nil (a body at the limit is not over it)", err)
	}
	if len(body) != limit {
		t.Errorf("read %d bytes, want %d", len(body), limit)
	}
}

func TestDo_BodyOverLimitReturnsErrResponseTooLarge(t *testing.T) {
	const limit = 1024
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := w.Write(make([]byte, limit*4)); err != nil {
			t.Errorf("writing test body failed: %v", err)
		}
	}))
	defer srv.Close()

	cfg := testConfig()
	cfg.MaxResponseBytes = limit
	resp, err := New(cfg).Do(newRequest(t, context.Background(), srv.URL))
	if err != nil {
		t.Fatalf("Do() failed: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("io.ReadAll() error = %v, want one matching ErrResponseTooLarge", err)
	}
	if len(body) != limit {
		t.Errorf("read %d bytes before the error, want exactly the limit (%d): the sentinel byte must never reach the caller", len(body), limit)
	}

	// Sticky: a second read must not report a clean end of body, which
	// is exactly how a truncation would get mistaken for a short document.
	if _, err := resp.Body.Read(make([]byte, 16)); !errors.Is(err, ErrResponseTooLarge) {
		t.Errorf("second Read() after the limit returned %v, want ErrResponseTooLarge again", err)
	}
}

func TestDo_BodyOverLimitReadsCleanlyUpToTheLimit(t *testing.T) {
	const limit = 1000
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := w.Write(make([]byte, limit*2)); err != nil {
			t.Errorf("writing test body failed: %v", err)
		}
	}))
	defer srv.Close()

	cfg := testConfig()
	cfg.MaxResponseBytes = limit
	resp, err := New(cfg).Do(newRequest(t, context.Background(), srv.URL))
	if err != nil {
		t.Fatalf("Do() failed: %v", err)
	}
	defer resp.Body.Close()

	buf := make([]byte, 100)
	var total int
	for {
		n, err := resp.Body.Read(buf)
		total += n
		if err != nil {
			if !errors.Is(err, ErrResponseTooLarge) {
				t.Fatalf("Read() at %d bytes returned %v, want ErrResponseTooLarge", total, err)
			}
			break
		}
		if total > limit {
			t.Fatalf("read %d bytes with no error, want at most %d", total, limit)
		}
	}
	if total != limit {
		t.Errorf("read %d bytes before the error, want %d: the error must not arrive early", total, limit)
	}
}

func TestDo_RetriesOn429AndRespectsRetryAfter(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := testConfig()
	cfg.Timeout = 10 * time.Second
	c := New(cfg)

	start := time.Now()
	resp, err := doWithin(t, 8*time.Second, c, newRequest(t, context.Background(), srv.URL))
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Do() failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("final status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("server received %d requests, want 2 (one 429, one retry)", got)
	}
	// The computed backoff for attempt 0 can never exceed backoffDelay(0, 1);
	// waiting materially longer than that is only explainable by the
	// Retry-After: 1 header having won.
	if elapsed < time.Second {
		t.Errorf("elapsed %s, want at least 1s: Retry-After: 1 was ignored in favor of the computed backoff (max %s)",
			elapsed, backoffDelay(0, 1))
	}
	if elapsed > 5*time.Second {
		t.Errorf("elapsed %s, want well under 5s for a 1-second Retry-After", elapsed)
	}
}

func TestDo_RetryAfterZeroOverridesComputedBackoff(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	start := time.Now()
	resp, err := doWithin(t, 5*time.Second, New(testConfig()), newRequest(t, context.Background(), srv.URL))
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Do() failed: %v", err)
	}
	defer resp.Body.Close()

	if got := calls.Load(); got != 2 {
		t.Errorf("server received %d requests, want 2", got)
	}
	// backoffDelay(0, 0) is the smallest wait the computed backoff can
	// ever produce, so finishing faster than that proves the header's
	// zero was used instead.
	if floor := backoffDelay(0, 0); elapsed >= floor {
		t.Errorf("elapsed %s, want under %s: Retry-After: 0 did not replace the computed backoff", elapsed, floor)
	}
}

func TestDo_RetriesOn503(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if _, err := io.WriteString(w, "recovered"); err != nil {
			t.Errorf("writing test body failed: %v", err)
		}
	}))
	defer srv.Close()

	resp, err := doWithin(t, 5*time.Second, New(testConfig()), newRequest(t, context.Background(), srv.URL))
	if err != nil {
		t.Fatalf("Do() failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("final status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("server received %d requests, want 2 (one 503, one retry)", got)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("io.ReadAll() failed: %v", err)
	}
	if string(body) != "recovered" {
		t.Errorf("body = %q, want %q: the retried response's body must reach the caller intact", body, "recovered")
	}
}

func TestDo_DoesNotRetryOn404(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	resp, err := doWithin(t, 5*time.Second, New(testConfig()), newRequest(t, context.Background(), srv.URL))
	if err != nil {
		t.Fatalf("Do() failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusNotFound)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("server received %d requests, want exactly 1: a 404 is an answer, not a transient failure", got)
	}
}

func TestDo_StopsRetryingAfterMaxRetriesAndReturnsLastResponse(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
		if _, err := io.WriteString(w, "still down"); err != nil {
			t.Errorf("writing test body failed: %v", err)
		}
	}))
	defer srv.Close()

	cfg := testConfig()
	cfg.MaxRetries = 2
	resp, err := doWithin(t, 5*time.Second, New(cfg), newRequest(t, context.Background(), srv.URL))
	if err != nil {
		t.Fatalf("Do() failed: %v", err)
	}
	defer resp.Body.Close()

	if got, want := calls.Load(), int64(cfg.MaxRetries+1); got != want {
		t.Errorf("server received %d requests, want %d (the first attempt plus MaxRetries)", got, want)
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want the last failure's %d", resp.StatusCode, http.StatusServiceUnavailable)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("io.ReadAll() on the exhausted response failed: %v", err)
	}
	if string(body) != "still down" {
		t.Errorf("body = %q, want %q: the final response's body must still be readable and size-limited", body, "still down")
	}
}

func TestDo_StopsRetryingAfterMaxRetriesOnNetworkError(t *testing.T) {
	cfg := testConfig()
	cfg.MaxRetries = 2
	url := fmt.Sprintf("http://%s/", unreachableAddr(t))

	start := time.Now()
	resp, err := doWithin(t, 10*time.Second, New(cfg), newRequest(t, context.Background(), url))
	elapsed := time.Since(start)

	if err == nil {
		resp.Body.Close()
		t.Fatal("Do() returned nil error for a refused connection, want the last failure")
	}
	if resp != nil {
		t.Error("Do() returned a non-nil response alongside an error")
	}
	if !strings.Contains(err.Error(), "httpclient: giving up after 3 attempt(s)") {
		t.Errorf("error = %q, want it to name the exhausted attempts and the last failure", err)
	}
	// Two backoffs happened between the three attempts; both have a
	// known floor, so anything faster means a retry was skipped.
	if floor := backoffDelay(0, 0) + backoffDelay(1, 0); elapsed < floor {
		t.Errorf("elapsed %s, want at least %s: the retries did not actually back off", elapsed, floor)
	}
}

func TestDo_CanceledContextDuringBackoffReturnsPromptly(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		// An hour of Retry-After: without context handling, Do would sit
		// here long past the test's patience.
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	resp, err := doWithin(t, 5*time.Second, New(testConfig()), newRequest(t, ctx, srv.URL))
	elapsed := time.Since(start)

	if err == nil {
		resp.Body.Close()
		t.Fatal("Do() returned nil error after its context was canceled")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want one matching context.Canceled", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("Do() took %s to notice cancellation, want prompt return", elapsed)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("server received %d requests, want 1: cancellation must stop the retries, not run them out", got)
	}
}

func TestDo_AlreadyCanceledContextMakesNoRequest(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	resp, err := doWithin(t, 5*time.Second, New(testConfig()), newRequest(t, ctx, srv.URL))
	if err == nil {
		resp.Body.Close()
		t.Fatal("Do() returned nil error for an already-canceled context")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want one matching context.Canceled", err)
	}
	if got := calls.Load(); got != 0 {
		t.Errorf("server received %d requests, want 0", got)
	}
}

func TestDo_FollowsRedirectsUnderTheLimit(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/start", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/end", http.StatusFound)
	})
	mux.HandleFunc("/end", func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.WriteString(w, "arrived"); err != nil {
			t.Errorf("writing test body failed: %v", err)
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := doWithin(t, 5*time.Second, New(testConfig()), newRequest(t, context.Background(), srv.URL+"/start"))
	if err != nil {
		t.Fatalf("Do() failed on a single redirect: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("io.ReadAll() failed: %v", err)
	}
	if string(body) != "arrived" {
		t.Errorf("body = %q, want %q", body, "arrived")
	}
}

func TestDo_StopsAfterMaxRedirects(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Redirect(w, r, "/next", http.StatusFound)
	}))
	defer srv.Close()

	cfg := testConfig()
	cfg.MaxRedirects = 3
	resp, err := doWithin(t, 5*time.Second, New(cfg), newRequest(t, context.Background(), srv.URL))

	if err == nil {
		resp.Body.Close()
		t.Fatal("Do() returned nil error on an endless redirect chain")
	}
	if !errors.Is(err, ErrTooManyRedirects) {
		t.Errorf("error = %v, want one matching ErrTooManyRedirects", err)
	}
	if !strings.HasPrefix(err.Error(), "httpclient: ") {
		t.Errorf("error = %q, want the package prefix", err)
	}
	// Exactly MaxRedirects+1 requests: the original request plus exactly
	// MaxRedirects redirects actually followed, then the next one refused
	// before it is sent. (CheckRedirect's via includes the original
	// request, so permitting exactly N redirects takes N+1 total
	// requests — this pins that the off-by-one found in review stays
	// fixed.) A redirect limit is a settled answer, so the retry loop
	// must not multiply the chain on top of this.
	if got, want := calls.Load(), int64(cfg.MaxRedirects)+1; got != want {
		t.Errorf("server received %d requests, want %d", got, want)
	}
}

// Regression test for the off-by-one found in adversarial review:
// MaxRedirects=1 must actually permit one redirect, not behave
// identically to MaxRedirects=0 (refusing every redirect).
func TestDo_MaxRedirectsOfOnePermitsExactlyOneRedirect(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/start", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/end", http.StatusFound)
	})
	mux.HandleFunc("/end", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfg := testConfig()
	cfg.MaxRedirects = 1
	resp, err := doWithin(t, 5*time.Second, New(cfg), newRequest(t, context.Background(), srv.URL+"/start"))
	if err != nil {
		t.Fatalf("Do() with MaxRedirects=1 failed on a single redirect: %v", err)
	}
	resp.Body.Close()
}

// Upper-bound half of the same regression: MaxRedirects=1 must permit
// the first redirect but refuse the second, not just "at least one".
func TestDo_MaxRedirectsOfOneRefusesASecondRedirect(t *testing.T) {
	var calls atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/start", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Redirect(w, r, "/middle", http.StatusFound)
	})
	mux.HandleFunc("/middle", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Redirect(w, r, "/end", http.StatusFound)
	})
	mux.HandleFunc("/end", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfg := testConfig()
	cfg.MaxRedirects = 1
	resp, err := doWithin(t, 5*time.Second, New(cfg), newRequest(t, context.Background(), srv.URL+"/start"))
	if err == nil {
		resp.Body.Close()
		t.Fatal("Do() with MaxRedirects=1 succeeded on a two-hop chain, want ErrTooManyRedirects")
	}
	if !errors.Is(err, ErrTooManyRedirects) {
		t.Errorf("error = %v, want one matching ErrTooManyRedirects", err)
	}
	if got, want := calls.Load(), int64(2); got != want {
		t.Errorf("server received %d requests, want %d (/start and /middle, /end never reached)", got, want)
	}
}

// MaxRedirects=0 is the value the off-by-one collided with: before the
// fix, MaxRedirects=1 behaved identically to this. Pin it explicitly.
func TestDo_MaxRedirectsOfZeroRefusesEveryRedirect(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Redirect(w, r, "/next", http.StatusFound)
	}))
	defer srv.Close()

	cfg := testConfig()
	cfg.MaxRedirects = 0
	resp, err := doWithin(t, 5*time.Second, New(cfg), newRequest(t, context.Background(), srv.URL))
	if err == nil {
		resp.Body.Close()
		t.Fatal("Do() with MaxRedirects=0 succeeded on a redirecting server, want ErrTooManyRedirects")
	}
	if !errors.Is(err, ErrTooManyRedirects) {
		t.Errorf("error = %v, want one matching ErrTooManyRedirects", err)
	}
	if got, want := calls.Load(), int64(1); got != want {
		t.Errorf("server received %d requests, want 1 (the original request only, no redirect followed)", got)
	}
}

func TestBackoffDelay_ExponentialJitteredAndCapped(t *testing.T) {
	tests := []struct {
		name    string
		attempt int
		jitter  float64
		want    time.Duration
	}{
		{"attempt 0 floor is half the base window", 0, 0, 100 * time.Millisecond},
		{"attempt 0 ceiling approaches the base window", 0, 0.999, 199*time.Millisecond + 900*time.Microsecond},
		{"attempt 1 doubles", 1, 0, 200 * time.Millisecond},
		{"attempt 2 doubles again", 2, 0, 400 * time.Millisecond},
		{"attempt 5 still exponential", 5, 0, 3200 * time.Millisecond},
		{"attempt 6 is clamped by the cap", 6, 0, backoffCap / 2},
		{"a huge attempt cannot overflow past the cap", 1000, 0, backoffCap / 2},
		{"a negative attempt is treated as the first", -1, 0, 100 * time.Millisecond},
		{"an out-of-range jitter falls back to the floor", 0, 1.5, 100 * time.Millisecond},
	}
	for _, tt := range tests {
		got := backoffDelay(tt.attempt, tt.jitter)
		// The jittered arithmetic is float-based; a millisecond of slack
		// keeps the test about the shape of the curve, not rounding.
		if diff := got - tt.want; diff > time.Millisecond || diff < -time.Millisecond {
			t.Errorf("%s: backoffDelay(%d, %v) = %s, want ~%s", tt.name, tt.attempt, tt.jitter, got, tt.want)
		}
		if got > backoffCap {
			t.Errorf("%s: backoffDelay(%d, %v) = %s, want at most the cap %s", tt.name, tt.attempt, tt.jitter, got, backoffCap)
		}
	}
}

func TestBackoffDelay_JitterSpreadsWithinTheWindow(t *testing.T) {
	const attempt = 3
	low, high := backoffDelay(attempt, 0), backoffDelay(attempt, 0.999)
	if low >= high {
		t.Fatalf("backoffDelay(%d, 0) = %s and backoffDelay(%d, 0.999) = %s; jitter must widen the window so concurrent callers do not retry in lockstep",
			attempt, low, attempt, high)
	}

	seen := make(map[time.Duration]struct{})
	for i := range 200 {
		d := backoffDelay(attempt, float64(i)/200)
		if d < low || d > high {
			t.Fatalf("backoffDelay(%d, %v) = %s, outside [%s, %s]", attempt, float64(i)/200, d, low, high)
		}
		seen[d] = struct{}{}
	}
	if len(seen) < 2 {
		t.Errorf("jitter produced %d distinct delays over 200 draws, want a spread", len(seen))
	}
}

func TestRetryAfterDelay_ParsesSecondsAndHTTPDate(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name   string
		header string
		want   time.Duration
		wantOK bool
	}{
		{"absent header", "", 0, false},
		{"whitespace only", "   ", 0, false},
		{"zero seconds", "0", 0, true},
		{"positive seconds", "30", 30 * time.Second, true},
		{"padded seconds", " 5 ", 5 * time.Second, true},
		{"negative seconds", "-5", 0, false},
		{"non-numeric garbage", "soon", 0, false},
		{"fractional seconds are not the RFC's format", "1.5", 0, false},
		{"future HTTP-date", now.Add(90 * time.Second).Format(http.TimeFormat), 90 * time.Second, true},
		{"HTTP-date at now", now.Format(http.TimeFormat), 0, true},
		{"past HTTP-date", now.Add(-time.Minute).Format(http.TimeFormat), 0, false},
	}
	for _, tt := range tests {
		got, ok := retryAfterDelay(tt.header, now)
		if ok != tt.wantOK {
			t.Errorf("%s: retryAfterDelay(%q) ok = %v, want %v", tt.name, tt.header, ok, tt.wantOK)
			continue
		}
		if ok && got != tt.want {
			t.Errorf("%s: retryAfterDelay(%q) = %s, want %s", tt.name, tt.header, got, tt.want)
		}
	}
}

func TestSleep_ReturnsContextErrorWhenAlreadyDone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// Both branches matter: a non-positive delay takes the fast path,
	// which must still observe the dead context.
	if err := sleep(ctx, 0); !errors.Is(err, context.Canceled) {
		t.Errorf("sleep(canceled, 0) = %v, want context.Canceled", err)
	}
	if err := sleep(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Errorf("sleep(canceled, 1h) = %v, want context.Canceled", err)
	}
}

func TestNewLimitedBody_NonPositiveLimitLeavesBodyUnwrapped(t *testing.T) {
	original := io.NopCloser(strings.NewReader("unbounded"))
	if got := newLimitedBody(original, 0); got != original {
		t.Errorf("newLimitedBody(body, 0) wrapped the body; a non-positive limit means no limit")
	}
}

func TestLimitedBody_CloseClosesTheOriginal(t *testing.T) {
	tracker := &closeTracker{Reader: strings.NewReader("payload")}
	body := newLimitedBody(tracker, 4)

	if _, err := io.ReadAll(body); !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("io.ReadAll() = %v, want ErrResponseTooLarge", err)
	}
	if err := body.Close(); err != nil {
		t.Fatalf("Close() failed: %v", err)
	}
	if !tracker.closed {
		t.Error("Close() did not close the underlying body; the connection would leak")
	}
}

type closeTracker struct {
	io.Reader
	closed bool
}

func (c *closeTracker) Close() error {
	c.closed = true
	return nil
}
