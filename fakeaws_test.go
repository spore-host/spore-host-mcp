package main

import (
	"bytes"
	"context"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	spawnclient "github.com/spore-host/spawn/pkg/aws"
)

// fakeAWS is an in-process AWS for the spawn tools: an http.RoundTripper that
// answers the EC2, STS and S3 calls those tools make and records what they
// sent. Every request stops here, so no test can reach AWS; an unexpected
// request fails the test that made it.
//
// It answers at the wire, not at an interface of ours, so the code under test
// is spawn's real client: ListInstances builds the real DescribeInstances
// filters, and the test reads them off the request (#63).
type fakeAWS struct {
	t       *testing.T
	account string

	mu        sync.Mutex
	instances []fakeInstance
	objects   map[string]string // "bucket/key" -> body
	// describeCalls holds, per DescribeInstances call, the instance-state-name
	// filter values it sent, or nil when it sent no such filter.
	describeCalls [][]string
	actions       []string
	mutated       []string // instance ids a Stop/Terminate/CreateTags named
	s3Gets        []string // "bucket/key"
}

type fakeInstance struct {
	id, name, state, instanceType, region string
	spot                                  bool
	tags                                  map[string]string
	launched                              time.Time
}

func newFakeAWS(t *testing.T) *fakeAWS {
	return &fakeAWS{t: t, account: "123456789012", objects: map[string]string{}}
}

// client is a spawn client whose every request goes to f.
func (f *fakeAWS) client() *spawnclient.Client {
	return spawnclient.NewClientFromConfig(aws.Config{
		Region: "us-west-2",
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: "AKIDFAKE", SecretAccessKey: "fake", Source: "fakeAWS"}, nil
		}),
		HTTPClient: &http.Client{Transport: f},
		Retryer:    func() aws.Retryer { return aws.NopRetryer{} },
	})
}

// install makes the spawn tools use f for the rest of the test.
func (f *fakeAWS) install(t *testing.T) {
	t.Helper()
	prev := spawnClient
	spawnClient = func(context.Context) (*spawnclient.Client, error) { return f.client(), nil }
	t.Cleanup(func() { spawnClient = prev })
}

func (f *fakeAWS) RoundTrip(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		body, _ = io.ReadAll(req.Body)
	}
	host := req.URL.Host
	switch {
	case strings.HasPrefix(host, "ec2."):
		form, _ := url.ParseQuery(string(body))
		return f.ec2(form)
	case strings.HasPrefix(host, "sts."):
		form, _ := url.ParseQuery(string(body))
		if form.Get("Action") == "GetCallerIdentity" {
			return xmlResponse(200, `<GetCallerIdentityResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><GetCallerIdentityResult><Arn>arn:aws:iam::`+f.account+`:user/test</Arn><UserId>AIDAFAKE</UserId><Account>`+f.account+`</Account></GetCallerIdentityResult><ResponseMetadata><RequestId>fake</RequestId></ResponseMetadata></GetCallerIdentityResponse>`), nil
		}
	case strings.Contains(host, ".s3.") || strings.HasPrefix(host, "s3."):
		if req.Method == http.MethodGet {
			return f.s3Get(req)
		}
	}
	f.fail("fakeAWS: unexpected request %s %s %s", req.Method, req.URL, body)
	return xmlResponse(400, `<Response><Errors><Error><Code>Unexpected</Code><Message>fakeAWS has no answer for this request</Message></Error></Errors></Response>`), nil
}

// fail reports a request the fake has no answer for. The package-wide default
// fake (TestMain) has no test to fail; it answers with an error, which the
// smoke tests that reach it expect.
func (f *fakeAWS) fail(format string, args ...any) {
	if f.t != nil {
		f.t.Errorf(format, args...)
	}
}

func (f *fakeAWS) ec2(form url.Values) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	action := form.Get("Action")
	f.actions = append(f.actions, action)
	switch action {
	case "DescribeInstances":
		var states []string
		hasStateFilter := false
		for i := 1; ; i++ {
			name := form.Get(fmt.Sprintf("Filter.%d.Name", i))
			if name == "" {
				break
			}
			if name != "instance-state-name" {
				continue
			}
			hasStateFilter = true
			for j := 1; ; j++ {
				v := form.Get(fmt.Sprintf("Filter.%d.Value.%d", i, j))
				if v == "" {
					break
				}
				states = append(states, v)
			}
		}
		if hasStateFilter && states == nil {
			states = []string{}
		}
		f.describeCalls = append(f.describeCalls, states)
		var items strings.Builder
		for _, in := range f.instances {
			if hasStateFilter && !contains(states, in.state) {
				continue
			}
			items.WriteString(in.xml())
		}
		return xmlResponse(200, `<DescribeInstancesResponse xmlns="http://ec2.amazonaws.com/doc/2016-11-15/"><requestId>fake</requestId><reservationSet>`+items.String()+`</reservationSet></DescribeInstancesResponse>`), nil
	case "StopInstances", "TerminateInstances", "CreateTags":
		for _, k := range []string{"InstanceId.1", "ResourceId.1"} {
			if id := form.Get(k); id != "" {
				f.mutated = append(f.mutated, id)
			}
		}
		return xmlResponse(200, `<`+action+`Response xmlns="http://ec2.amazonaws.com/doc/2016-11-15/"><requestId>fake</requestId><return>true</return></`+action+`Response>`), nil
	}
	f.fail("fakeAWS: unexpected EC2 action %q", action)
	return xmlResponse(400, `<Response><Errors><Error><Code>Unexpected</Code><Message>no answer</Message></Error></Errors></Response>`), nil
}

func (f *fakeAWS) s3Get(req *http.Request) (*http.Response, error) {
	host, path := req.URL.Host, strings.TrimPrefix(req.URL.Path, "/")
	var bucket, key string
	if i := strings.Index(host, ".s3."); i > 0 {
		bucket, key = host[:i], path
	} else {
		bucket, key, _ = strings.Cut(path, "/")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.s3Gets = append(f.s3Gets, bucket+"/"+key)
	if body, ok := f.objects[bucket+"/"+key]; ok {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), ContentLength: int64(len(body))}, nil
	}
	return xmlResponse(404, `<Error><Code>NoSuchKey</Code><Message>The specified key does not exist.</Message></Error>`), nil
}

func (in fakeInstance) xml() string {
	region := in.region
	if region == "" {
		region = "us-west-2"
	}
	launched := in.launched
	if launched.IsZero() {
		launched = time.Now().Add(-30 * time.Minute)
	}
	var tags strings.Builder
	tag := func(k, v string) {
		fmt.Fprintf(&tags, "<item><key>%s</key><value>%s</value></item>", html.EscapeString(k), html.EscapeString(v))
	}
	tag("Name", in.name)
	tag("spawn:managed", "true")
	for k, v := range in.tags {
		tag(k, v)
	}
	lifecycle := ""
	if in.spot {
		lifecycle = "<instanceLifecycle>spot</instanceLifecycle>"
	}
	return fmt.Sprintf(`<item><reservationId>r-%s</reservationId><instancesSet><item><instanceId>%s</instanceId><instanceType>%s</instanceType><instanceState><code>0</code><name>%s</name></instanceState><placement><availabilityZone>%sa</availabilityZone></placement><launchTime>%s</launchTime>%s<tagSet>%s</tagSet></item></instancesSet></item>`,
		in.id, in.id, in.instanceType, in.state, region, launched.UTC().Format(time.RFC3339), lifecycle, tags.String())
}

func xmlResponse(code int, body string) *http.Response {
	return &http.Response{
		StatusCode:    code,
		Header:        http.Header{"Content-Type": {"text/xml"}},
		Body:          io.NopCloser(bytes.NewReader([]byte(body))),
		ContentLength: int64(len(body)),
	}
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// TestMain points every spawn tool at an empty fakeAWS unless a test installs
// its own, so no test in this package can reach AWS through spawnClient — the
// handler smoke tests used to call real AWS when the environment had
// credentials.
func TestMain(m *testing.M) {
	empty := &fakeAWS{account: "123456789012", objects: map[string]string{}}
	spawnClient = func(context.Context) (*spawnclient.Client, error) { return empty.client(), nil }
	os.Exit(m.Run())
}
