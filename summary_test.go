package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/spore-host/lagotto/pkg/watcher"
	"github.com/spore-host/spawn/pkg/taskproto"
	truffleaws "github.com/spore-host/truffle/pkg/aws"
	"github.com/spore-host/truffle/pkg/find"
)

// The summary-line helper (#75): one line, no Markdown, no account ID, capped.
func TestSummaryLine(t *testing.T) {
	long := strings.Repeat("x", maxSummaryRunes+40)
	for _, tc := range []struct{ name, in, want string }{
		{"plain", "r8g.2xlarge: 8 vCPU", "r8g.2xlarge: 8 vCPU"},
		{"newlines collapse", "a\nb\n\n  c\t d", "a b c d"},
		{"markdown stripped", "**g5.xlarge** costs `$1`", "g5.xlarge costs $1"},
		{"account in bucket", "record at s3://spawn-results-123456789012-us-west-2/x", "record at s3://spawn-results-<account>-us-west-2/x"},
		{"account in ARN", "arn:aws:iam::123456789012:user/a", "arn:aws:iam::<account>:user/a"},
		{"two accounts one apart", "123456789012:210987654321", "<account>:<account>"},
		{"13 digits is not an account", "id 1234567890123", "id 1234567890123"},
		{"11 digits is not an account", "id 12345678901", "id 12345678901"},
		{"clipped", long, strings.Repeat("x", maxSummaryRunes-1) + "…"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := summaryLine(tc.in)
			if got != tc.want {
				t.Errorf("summaryLine(%q)\n got %q\nwant %q", tc.in, got, tc.want)
			}
			if n := utf8.RuneCountInString(got); n > maxSummaryRunes {
				t.Errorf("summary is %d runes, over the %d cap", n, maxSummaryRunes)
			}
		})
	}
}

func TestWithSummary(t *testing.T) {
	for _, tc := range []struct{ name, summary, body, want string }{
		{"summary then blank line then body", "2 running", "Found 2:\n\n• a\n", "2 running\n\nFound 2:\n\n• a"},
		{"no body: the line alone", "✅ a (i-1) terminated.", "", "✅ a (i-1) terminated."},
		{"blank body: the line alone", "No watches.", "\n\n", "No watches."},
		{"the summary is made a line", "a\nb", "body", "a b\n\nbody"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := withSummary(tc.summary, tc.body)
			if res.IsError {
				t.Fatal("withSummary built an error result")
			}
			if got := resultText(res); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestJoinWithin(t *testing.T) {
	items := []string{"aaaa", "bbbb", "cccc", "dddd"}
	for _, tc := range []struct {
		budget int
		want   string
	}{
		{100, "aaaa, bbbb, cccc, dddd"},
		{22, "aaaa, bbbb, cccc, dddd"},
		{21, "aaaa, bbbb, +2 more"},
		{1, "aaaa, +3 more"},
	} {
		if got := joinWithin(items, tc.budget); got != tc.want {
			t.Errorf("joinWithin(budget %d) = %q, want %q", tc.budget, got, tc.want)
		}
	}
}

func TestSummaryFormatters(t *testing.T) {
	for _, tc := range []struct{ got, want string }{
		{money(0.16), "$0.16"},
		{money(0.0042), "$0.0042"},
		{money(0), "$0.00"},
		{clockDuration(19*time.Minute + 50*time.Second), "19m 50s"},
		{clockDuration(45 * time.Second), "45s"},
		{clockDuration(time.Hour + 2*time.Minute + 3*time.Second), "1h 2m 3s"},
		{shortAge(12 * time.Minute), "12m"},
		{shortAge(3*time.Hour + 20*time.Minute), "3h"},
		{shortAge(50 * time.Hour), "2d"},
	} {
		if tc.got != tc.want {
			t.Errorf("got %q, want %q", tc.got, tc.want)
		}
	}
}

// TestOnlyWithSummaryBuildsTextResults keeps #75's rule in one place: every
// successful result is built by withSummary, so no handler can return a text
// result whose first line skipped the summary rules.
func TestOnlyWithSummaryBuildsTextResults(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") || path == "summary.go" {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		mcpName := ""
		for _, imp := range f.Imports {
			if p, _ := strconv.Unquote(imp.Path.Value); p == "github.com/mark3labs/mcp-go/mcp" {
				mcpName = "mcp"
				if imp.Name != nil {
					mcpName = imp.Name.Name
				}
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if x, ok := sel.X.(*ast.Ident); ok && x.Name == mcpName && strings.HasPrefix(sel.Sel.Name, "NewToolResult") && sel.Sel.Name != "NewToolResultError" {
				t.Errorf("%s: %s.%s builds a result without withSummary (#75)", fset.Position(sel.Pos()), mcpName, sel.Sel.Name)
			}
			return true
		})
	}
}

// --- truffle: built from values, since truffle's tools build their own AWS
// clients and cannot be pointed at the fake.

func TestFindSummary(t *testing.T) {
	r8g := truffleaws.InstanceTypeResult{InstanceType: "r8g.2xlarge", Region: "us-west-2", VCPUs: 8, MemoryMiB: 65536, Architecture: "arm64",
		OnDemandPrice: 0.4713, AvailableAZs: []string{"us-west-2a", "us-west-2b", "us-west-2c", "us-west-2d"}}
	r8gEast := r8g
	r8gEast.Region, r8gEast.OnDemandPrice, r8gEast.AvailableAZs = "us-east-1", 0.4500, []string{"us-east-1a"}
	m8g := truffleaws.InstanceTypeResult{InstanceType: "m8g.4xlarge", Region: "us-west-2", VCPUs: 16, MemoryMiB: 65536, Architecture: "arm64", OnDemandPrice: 0.7181}
	g5 := truffleaws.InstanceTypeResult{InstanceType: "g5.xlarge", Region: "us-east-1", VCPUs: 4, MemoryMiB: 16384, Architecture: "x86_64", GPUs: 1, GPUModel: "A10G", OnDemandPrice: 1.006}
	noPrice := truffleaws.InstanceTypeResult{InstanceType: "x8g.large", Region: "us-west-2", VCPUs: 2, MemoryMiB: 32768, Architecture: "arm64"}
	spot := map[string]truffleaws.SpotPriceResult{"r8g.2xlarge": {InstanceType: "r8g.2xlarge", SpotPrice: 0.1842}}

	for _, tc := range []struct {
		name    string
		query   string
		results []truffleaws.InstanceTypeResult
		total   int
		spot    map[string]truffleaws.SpotPriceResult
		want    string
	}{
		{"one match", "r8g.2xlarge", []truffleaws.InstanceTypeResult{r8g}, 1, nil,
			"r8g.2xlarge: 8 vCPU, 64 GiB, arm64; offered in us-west-2 (a, b, c, d); $0.4713/hr On-Demand"},
		{"one match with Spot", "r8g.2xlarge", []truffleaws.InstanceTypeResult{r8g}, 1, spot,
			"r8g.2xlarge: 8 vCPU, 64 GiB, arm64; offered in us-west-2 (a, b, c, d); $0.4713/hr On-Demand, $0.1842/hr Spot"},
		{"one type in two regions", "r8g.2xlarge", []truffleaws.InstanceTypeResult{r8g, r8gEast}, 2, nil,
			"r8g.2xlarge: 8 vCPU, 64 GiB, arm64; offered in us-west-2 (a, b, c, d), us-east-1 (a); $0.4500/hr On-Demand in us-east-1"},
		{"one GPU match", "a10g", []truffleaws.InstanceTypeResult{g5}, 1, nil,
			"g5.xlarge: 4 vCPU, 16 GiB, x86_64, 1× A10G GPU; offered in us-east-1; $1.0060/hr On-Demand"},
		{"several matches", "graviton 64gb", []truffleaws.InstanceTypeResult{m8g, r8g}, 2, nil,
			`2 matches for "graviton 64gb"; cheapest On-Demand: r8g.2xlarge at $0.4713/hr in us-west-2`},
		{"several, clipped by the cap", "arm64", []truffleaws.InstanceTypeResult{m8g, r8g}, 300, nil,
			`300 matches for "arm64" (top 2 shown); cheapest shown On-Demand: r8g.2xlarge at $0.4713/hr in us-west-2`},
		{"several, no prices", "arm64", []truffleaws.InstanceTypeResult{noPrice, {InstanceType: "x8g.xlarge", Region: "us-west-2"}}, 2, nil,
			`2 matches for "arm64"; no On-Demand price available`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := summaryLine(findSummary(tc.query, tc.results, tc.total, tc.spot)); got != tc.want {
				t.Errorf("\n got %q\nwant %q", got, tc.want)
			}
		})
	}
}

// No match keeps the existing message as the first line; the notes on how the
// query was read move below it.
func TestFindNoMatchLeadsWithTheMessage(t *testing.T) {
	pq, err := find.ParseQuery("graviton 64gb frobnicate")
	if err != nil {
		t.Fatal(err)
	}
	out := resultText(findNoMatch("graviton 64gb frobnicate", []string{"us-west-2"}, pq))
	lines := strings.Split(out, "\n")
	if want := `No instance types found matching "graviton 64gb frobnicate" in us-west-2.`; lines[0] != want {
		t.Errorf("first line %q, want %q", lines[0], want)
	}
	if len(lines) < 3 || lines[1] != "" || !strings.Contains(out, "ignored unknown term(s): frobnicate") {
		t.Errorf("the query notes should follow a blank line:\n%s", out)
	}
}

func TestQuotaSummary(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		vcpus, quota, usage      int32
		known, canLaunch, hasCmd bool
		spot                     bool
		msg                      string
		want                     string
	}{
		{"allowed", 8, 128, 8, true, true, false, false, "",
			"✅ r8g.2xlarge On-Demand in us-west-2: allowed — needs 8 vCPU, 120 of 128 available"},
		{"blocked", 8, 32, 28, true, false, true, false, "Need 8 vCPUs, only 4 available",
			"❌ r8g.2xlarge On-Demand in us-west-2: blocked — needs 8 vCPU, 4 of 32 available (increase command below)"},
		{"blocked, Spot, no command", 8, 32, 28, true, false, false, true, "Need 8 vCPUs, only 4 available",
			"❌ r8g.2xlarge Spot in us-west-2: blocked — needs 8 vCPU, 4 of 32 available"},
		{"quota unknown", 8, 0, 0, false, false, false, false, "could not determine the On-Demand vCPU quota for standard instances",
			"❌ r8g.2xlarge On-Demand in us-west-2: blocked — needs 8 vCPU; could not determine the On-Demand vCPU quota for standard instances"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			qt := "On-Demand"
			if tc.spot {
				qt = "Spot"
			}
			got := summaryLine(quotaSummary("r8g.2xlarge", qt, "us-west-2", tc.vcpus, tc.quota, tc.usage, tc.known, tc.canLaunch, tc.hasCmd, tc.msg))
			if got != tc.want {
				t.Errorf("\n got %q\nwant %q", got, tc.want)
			}
		})
	}
}

func TestSpotSummary(t *testing.T) {
	prices := []truffleaws.SpotPriceResult{
		{InstanceType: "g5.xlarge", Region: "us-east-1", AvailabilityZone: "us-east-1a", SpotPrice: 0.45, OnDemandPrice: 1.006, SavingsPercent: 55},
		{InstanceType: "g5.xlarge", Region: "us-east-1", AvailabilityZone: "us-east-1b", SpotPrice: 0.38, OnDemandPrice: 1.006, SavingsPercent: 62},
		{InstanceType: "g5.xlarge", Region: "us-west-2", AvailabilityZone: "us-west-2c", SpotPrice: 0.41},
	}
	for _, tc := range []struct {
		name   string
		prices []truffleaws.SpotPriceResult
		want   string
	}{
		{"prices", prices, "g5.xlarge Spot: lowest $0.3800/hr in us-east-1b (62% below On-Demand $1.0060/hr); 3 price(s) across 2 region(s)"},
		{"none", nil, "No Spot price data available for g5.xlarge."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := summaryLine(spotSummary("g5.xlarge", tc.prices)); got != tc.want {
				t.Errorf("\n got %q\nwant %q", got, tc.want)
			}
		})
	}
}

// --- lagotto list/status: the fake answers no DynamoDB reads, so these are
// built from values too.

func TestWatchListSummary(t *testing.T) {
	watches := []watcher.Watch{
		{WatchID: "w-1a2b3c4d", InstanceTypePattern: "r8g.2xlarge", Status: watcher.StatusActive},
		{WatchID: "w-5e6f7a8b", InstanceTypePattern: "g5.*", Status: watcher.StatusMatched},
	}
	for _, tc := range []struct {
		name    string
		all     bool
		project string
		want    string
	}{
		{"active", false, "", "2 active watch(es): w-1a2b3c4d r8g.2xlarge [active], w-5e6f7a8b g5.* [matched]"},
		{"all, in a project", true, "era5", `2 watch(es) in project "era5": w-1a2b3c4d r8g.2xlarge [active], w-5e6f7a8b g5.* [matched]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := summaryLine(watchListSummary(watches, tc.all, tc.project)); got != tc.want {
				t.Errorf("\n got %q\nwant %q", got, tc.want)
			}
		})
	}
}

func TestWatchSummary(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	active := watcher.Watch{WatchID: "w-1a2b3c4d", Status: watcher.StatusActive, InstanceTypePattern: "r8g.2xlarge",
		Regions: []string{"us-west-2"}, Action: watcher.ActionNotify, Spot: true, ExpiresAt: now.Add(23 * time.Hour)}
	matched := active
	matched.Status = watcher.StatusMatched
	matched.MatchCount = 1
	matched.LastMatch = &watcher.MatchResult{InstanceType: "r8g.2xlarge", Region: "us-west-2", AvailabilityZone: "us-west-2b", Price: 0.1842, IsSpot: true}
	for _, tc := range []struct {
		name string
		w    watcher.Watch
		want string
	}{
		{"active", active, "Watch w-1a2b3c4d active: r8g.2xlarge Spot in us-west-2, action notify; 0 match(es), expires in 23h 0m"},
		{"matched", matched, "Watch w-1a2b3c4d matched: r8g.2xlarge Spot in us-west-2, action notify; last match r8g.2xlarge in us-west-2b at $0.1842/hr"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := summaryLine(watchSummary(&tc.w, now)); got != tc.want {
				t.Errorf("\n got %q\nwant %q", got, tc.want)
			}
		})
	}
}

// The cut-short reasons on the spawn_status line.
func TestShortEndReason(t *testing.T) {
	for _, tc := range []struct {
		rec  taskproto.CompletionRecord
		want string
	}{
		{taskproto.CompletionRecord{State: taskproto.StateCompleted}, ""},
		{taskproto.CompletionRecord{State: "failed", RetryClass: taskproto.RetryAppError}, ""},
		{taskproto.CompletionRecord{State: "failed", TerminalReason: taskproto.ExitTTLExpired}, "its TTL expired"},
		{taskproto.CompletionRecord{State: "failed", TerminalReason: taskproto.ExitSpotInterruption}, "its Spot instance was interrupted"},
		{taskproto.CompletionRecord{State: "failed", RetryClass: taskproto.RetryStagingError}, "an input failed to stage in"},
	} {
		if got := shortEndReason(&tc.rec); got != tc.want {
			t.Errorf("shortEndReason(%+v) = %q, want %q", tc.rec, got, tc.want)
		}
	}
}
