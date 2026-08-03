// This file asserts on repo wiring rather than on code.
//
// It exists because wiring is what rots: a CI step is a one-line deletion whose
// absence is completely silent — nothing fails, the tree simply starts drifting
// again, and nobody notices until an unrelated PR picks up a reformatting diff.
// This tree was clean when the gate landed, which is exactly why it is worth
// asserting on — the siblings drifted for months without a gate (spawn#484 had
// three files unformatted, truffle#122 had seven).
package main

import (
	"os"
	"strings"
	"testing"
)

// TestCIGatesFormatting checks that the formatting gate is still wired into CI,
// and that it REPORTS drift rather than fixing it.
//
// The second half is the subtle one. `gofmt -w` rewrites files and exits 0, so a
// "gate" built on it can only ever report success — green on a dirty tree
// forever, indistinguishable from having no gate at all. Only `gofmt -l`/`-d`
// can fail. This repo has no Makefile, so the gate is inline in the workflow.
func TestCIGatesFormatting(t *testing.T) {
	data, err := os.ReadFile(".github/workflows/ci.yml")
	if err != nil {
		t.Fatalf("read ci.yml: %v", err)
	}
	step, ok := workflowStep(string(data), "Format gate")
	if !ok {
		t.Fatal("the CI workflow has no 'Format gate' step; without it nothing " +
			"prevents unformatted code from reaching main (#26)")
	}
	if !strings.Contains(step, "gofmt -l") {
		t.Error("the Format gate does not run 'gofmt -l'; it must LIST offenders to be able to fail")
	}
	// Commands only, not message text: the step's error message tells the reader
	// to "run 'gofmt -w' on them", which is advice, not an invocation.
	if strings.Contains(unquote(step), "gofmt -w") {
		t.Error("the Format gate runs 'gofmt -w': that rewrites files and always exits 0, " +
			"so it reports success on a dirty tree. A gate must report, not fix.")
	}
	if !strings.Contains(step, "exit 1") {
		t.Error("the Format gate never exits non-zero, so CI can't fail on drift")
	}
}

// workflowStep returns the YAML block for the step named name: from its "- name:"
// line up to the next line at the same indentation starting a new list item.
// Scoping to one step matters — otherwise an assertion could be satisfied by an
// unrelated step elsewhere in the file.
func workflowStep(yaml, name string) (string, bool) {
	lines := strings.Split(yaml, "\n")
	start := -1
	var indent string
	for i, line := range lines {
		if strings.HasSuffix(strings.TrimSpace(line), "name: "+name) &&
			strings.HasPrefix(strings.TrimSpace(line), "- ") {
			start = i
			indent = line[:strings.Index(line, "- ")]
			break
		}
	}
	if start < 0 {
		return "", false
	}
	for i := start + 1; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], indent+"- ") {
			return strings.Join(lines[start:i], "\n"), true
		}
	}
	return strings.Join(lines[start:], "\n"), true
}

// unquote strips single- and double-quoted spans, leaving roughly the commands.
// Crude, but the question it answers is narrow: does the step RUN something, or
// merely mention it in a message?
func unquote(s string) string {
	var b strings.Builder
	var quote rune
	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			}
		case r == '\'' || r == '"':
			quote = r
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
