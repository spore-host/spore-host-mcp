package main

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// The hermetic guard (#79): any AWS client this package's tests build WITHOUT
// going through a fake fails the test binary, loudly, instead of reaching
// whatever account the developer's environment points at.
//
// fakeAWS covers the spawn and lagotto tools through the spawnClient seam. It
// cannot cover a client built some other way: truffle's tools build their own
// (truffleaws.NewClient, quotas.NewClient), and before #79 the lagotto tools
// did too. In CI, which has no credentials, such a client simply fails; on a
// developer's machine it signs real requests (lagotto#214: a lagotto unit test
// once wrote a real watch to a live account). So TestMain also installs this
// environment, which:
//
//   - strips every AWS_* and SPORE_* variable and sets obviously fake static
//     credentials, AWS_CONFIG_FILE / AWS_SHARED_CREDENTIALS_FILE to /dev/null,
//     AWS_EC2_METADATA_DISABLED, and XDG_CONFIG_HOME to /dev/null (the shared
//     spore.host config file can name a profile), so nothing can authenticate
//     as a real principal even if a request escapes;
//   - points AWS_ENDPOINT_URL at a loopback recorder, which catches every SDK
//     client built by config.LoadDefaultConfig;
//   - points HTTP(S)_PROXY at the same recorder, with NO_PROXY reduced to
//     loopback, which catches clients built from a bare aws.Config and IMDS.
//     Go never proxies loopback, and fakeAWS is an in-process RoundTripper, so
//     neither is affected.
//
// The recorder refuses every request (HTTP 403), and the binary fails after
// the run, naming each one.
//
// Ported from lagotto's internal/hermetic (lagotto v0.67.0, itself a port of
// spawn's internal/hermetic, spawn#968). It is internal there, so it cannot be
// imported; this is the same environment, trimmed to the TestMain half.

const hermeticFakeAccessKeyID = "hermetic-test-key-id"

const hermeticLoopback = "localhost,127.0.0.1,::1"

func hermeticDropped(k string) bool {
	u := strings.ToUpper(k)
	return strings.HasPrefix(u, "AWS_") || strings.HasPrefix(u, "SPORE_") ||
		u == "XDG_CONFIG_HOME" ||
		u == "HTTP_PROXY" || u == "HTTPS_PROXY" || u == "ALL_PROXY" || u == "NO_PROXY"
}

func hermeticSettings(recorderURL string) []string {
	return []string{
		"AWS_ACCESS_KEY_ID=" + hermeticFakeAccessKeyID,
		"AWS_SECRET_ACCESS_KEY=hermetic-test-not-a-real-secret",
		"AWS_REGION=us-west-2",
		"AWS_CONFIG_FILE=/dev/null",
		"AWS_SHARED_CREDENTIALS_FILE=/dev/null",
		"AWS_EC2_METADATA_DISABLED=true",
		"AWS_ENDPOINT_URL=" + recorderURL,
		"XDG_CONFIG_HOME=/dev/null",
		"HTTP_PROXY=" + recorderURL,
		"HTTPS_PROXY=" + recorderURL,
		"http_proxy=" + recorderURL,
		"https_proxy=" + recorderURL,
		"NO_PROXY=" + hermeticLoopback,
		"no_proxy=" + hermeticLoopback,
	}
}

// hermeticRecorder is a loopback HTTP server that records and refuses every
// request.
type hermeticRecorder struct {
	URL string

	srv  *http.Server
	mu   sync.Mutex
	seen []string
}

func startHermeticRecorder() (*hermeticRecorder, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	r := &hermeticRecorder{URL: "http://" + ln.Addr().String()}
	r.srv = &http.Server{Handler: r, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = r.srv.Serve(ln) }()
	return r, nil
}

func (r *hermeticRecorder) Close() { _ = r.srv.Close() }

var (
	hermeticSigV4Service = regexp.MustCompile(`Credential=[^/]+/\d+/[^/]+/([^/]+)/aws4_request`)
	hermeticQueryAction  = regexp.MustCompile(`(?:^|&)Action=([A-Za-z0-9]+)`)
)

func (r *hermeticRecorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	d := hermeticDescribe(req)
	r.mu.Lock()
	r.seen = append(r.seen, d)
	r.mu.Unlock()
	http.Error(w, "spore-host-mcp hermetic test environment: network access refused (#79)", http.StatusForbidden)
}

// hermeticDescribe renders a request so the failure says what was attempted.
func hermeticDescribe(req *http.Request) string {
	if req.Method == http.MethodConnect {
		return "HTTPS CONNECT " + req.Host
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s", req.Method, req.URL.String())
	if req.URL.Host == "" {
		b.WriteString(" (via AWS_ENDPOINT_URL)")
	}
	if m := hermeticSigV4Service.FindStringSubmatch(req.Header.Get("Authorization")); m != nil {
		fmt.Fprintf(&b, " service=%s", m[1])
	}
	if t := req.Header.Get("X-Amz-Target"); t != "" {
		fmt.Fprintf(&b, " target=%s", t)
	}
	if req.Body != nil {
		body, _ := io.ReadAll(io.LimitReader(req.Body, 4096))
		if m := hermeticQueryAction.FindStringSubmatch(string(body)); m != nil {
			fmt.Fprintf(&b, " action=%s", m[1])
		}
	}
	return b.String()
}

// take returns, sorted, and forgets what the recorder has seen. The guard's
// own self-test uses it to clear the request it makes on purpose.
func (r *hermeticRecorder) take() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.seen
	r.seen = nil
	sort.Strings(out)
	return out
}

// report writes the failure for a run that reached the recorder and reports
// whether there was anything to report.
func (r *hermeticRecorder) report(w io.Writer) bool {
	reqs := r.take()
	if len(reqs) == 0 {
		return false
	}
	_, _ = fmt.Fprintf(w, "\nHERMETIC GATE FAILED (#79): this test binary made %d network request(s) that would have left this machine.\n", len(reqs))
	_, _ = fmt.Fprintln(w, "A unit test must not reach AWS: with real credentials these are real control-plane calls")
	_, _ = fmt.Fprintln(w, "(lagotto#214: one wrote a real watch to a live account). Build the client through the")
	_, _ = fmt.Fprintln(w, "spawnClient seam and install a fakeAWS (fakeaws_test.go), or point it at an httptest server.")
	for _, q := range reqs {
		_, _ = fmt.Fprintf(w, "  - %s\n", q)
	}
	return true
}

// hermeticGuard is the running recorder, for the guard's self-test.
var hermeticGuard *hermeticRecorder

// installHermeticEnv starts the recorder and replaces this process's AWS
// environment with the hermetic one. The caller closes the recorder.
func installHermeticEnv() (*hermeticRecorder, error) {
	rec, err := startHermeticRecorder()
	if err != nil {
		return nil, err
	}
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); hermeticDropped(k) {
			_ = os.Unsetenv(k)
		}
	}
	for _, kv := range hermeticSettings(rec.URL) {
		k, v, _ := strings.Cut(kv, "=")
		_ = os.Setenv(k, v)
	}
	return rec, nil
}
