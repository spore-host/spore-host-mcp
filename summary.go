package main

import (
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mark3labs/mcp-go/mcp"
)

// Every successful tool result leads with a one-line summary (#75): the line
// that answers the question the call was asked, then a blank line, then the
// tool's detailed output as it was before. Agent clients increasingly show a
// tool call collapsed to its first line or two (Kiro CLI does), so that line
// is what a reader sees.
//
// The rules for the line, from #75:
//   - one line, no Markdown;
//   - it leads with the outcome: allowed/blocked, launched/running/completed/
//     failed, the price, or the count;
//   - it carries the cost whenever the result has one or it can be derived;
//   - it never carries an AWS account ID (bucket names and ARNs stay in the
//     detail below).
//
// Each handler builds its line from the values it already has, and returns
// through withSummary, which enforces the rules above in one place. Error
// results are not summarised: their first line is already the error.

// maxSummaryRunes is the hard cap on a summary line. #75 asks for "at most
// ~120 characters"; the formats aim at that for ordinary names, and this cap,
// a little looser, only stops a long name or a long list from making the line
// wrap into a paragraph. A clipped line ends in "…".
const maxSummaryRunes = 160

// accountIDPattern matches a 12-digit AWS account ID standing on its own: not
// part of a longer run of digits.
var accountIDPattern = regexp.MustCompile(`(^|[^0-9])[0-9]{12}([^0-9]|$)`)

// withSummary is the result every successful tool returns: summary as the
// first line, then a blank line, then body. An empty body gives the summary
// alone, for a tool whose whole answer fits on the line.
func withSummary(summary, body string) *mcp.CallToolResult {
	line := summaryLine(summary)
	body = strings.TrimRight(body, "\n")
	if strings.TrimSpace(body) == "" {
		return mcp.NewToolResultText(line)
	}
	return mcp.NewToolResultText(line + "\n\n" + body)
}

// summaryLine makes s a valid summary line: whitespace (newlines included)
// collapsed to single spaces, Markdown emphasis and code marks removed, any
// account ID replaced, and the result clipped to maxSummaryRunes.
func summaryLine(s string) string {
	s = strings.NewReplacer("**", "", "__", "", "`", "").Replace(s)
	s = strings.Join(strings.Fields(s), " ")
	// The pattern consumes the neighbouring character, so two IDs separated by
	// one character need a second pass.
	for accountIDPattern.MatchString(s) {
		s = accountIDPattern.ReplaceAllString(s, "${1}<account>${2}")
	}
	if utf8.RuneCountInString(s) > maxSummaryRunes {
		r := []rune(s)
		s = strings.TrimRight(string(r[:maxSummaryRunes-1]), " ,;:—-") + "…"
	}
	return s
}

// joinWithin joins items with ", " and stops adding them before the joined
// list would pass budget runes, ending it with "+N more" for the rest. At
// least one item is always shown.
func joinWithin(items []string, budget int) string {
	if all := strings.Join(items, ", "); utf8.RuneCountInString(all) <= budget {
		return all
	}
	var sb strings.Builder
	for i, it := range items {
		sep := ""
		if i > 0 {
			sep = ", "
		}
		rest := len(items) - i
		more := ""
		if rest > 1 {
			more = fmt.Sprintf(", +%d more", rest-1)
		}
		if i > 0 && utf8.RuneCountInString(sb.String()+sep+it+more) > budget {
			fmt.Fprintf(&sb, ", +%d more", rest)
			return sb.String()
		}
		sb.WriteString(sep + it)
	}
	return sb.String()
}

// money formats a dollar amount for a summary line: cents for anything a cent
// or more, four places below that, so a $0.004 task does not read as $0.00.
func money(v float64) string {
	if v > 0 && v < 0.01 {
		return fmt.Sprintf("$%.4f", v)
	}
	return fmt.Sprintf("$%.2f", v)
}

// shortAge is a duration in its largest whole unit (12m, 3h, 2d), for a list
// where the exact age is in the detail below.
func shortAge(d time.Duration) string {
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours())/24)
	}
}

// clockDuration is a duration to the second without a zero leading unit:
// 45s, 19m 50s, 1h 2m 3s.
func clockDuration(d time.Duration) string {
	d = d.Round(time.Second)
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	switch {
	case h > 0:
		return fmt.Sprintf("%dh %dm %ds", h, m, s)
	case m > 0:
		return fmt.Sprintf("%dm %ds", m, s)
	default:
		return fmt.Sprintf("%ds", s)
	}
}
