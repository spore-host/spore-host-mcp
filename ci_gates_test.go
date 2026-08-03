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
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
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

// repoRoot is the repo root relative to this test's package directory.
func repoRoot() string { return "." }

// dotGithub joins a path under the repo's .github directory.
func dotGithub(rel string) string { return filepath.Join(repoRoot(), ".github", rel) }

// TestDependabotCoversEveryAction is the other half of pinning actions to SHAs.
//
// A pin closes the mutable-tag hole but opens a staleness one: a SHA never moves,
// including past a security fix, and unlike `@v6` nothing updates it for you. So
// pinning is only safe if something bumps the pins — here, Dependabot. Nothing did
// before: `actions/checkout@v6` had already moved upstream while this repo went on
// pinning the older commit, silently.
//
// The check that matters is coverage: an ecosystem entry whose group patterns
// don't match an action leaves that action outside the grouped PR, silently. The
// pattern is "*" rather than "actions/*" precisely because the release-signing
// actions (goreleaser, cosign-installer, attest-build-provenance) aren't under
// actions/ — and those are the last ones that should quietly freeze.
func TestDependabotCoversEveryAction(t *testing.T) {
	data, err := os.ReadFile(dotGithub("dependabot.yml"))
	if err != nil {
		t.Fatalf("read dependabot.yml: %v (CI's actions are pinned to SHAs; without "+
			"Dependabot nothing ever bumps them)", err)
	}
	cfg, err := parseDependabot(data)
	if err != nil {
		t.Fatalf("dependabot.yml is not valid YAML: %v", err)
	}
	if cfg.Version != 2 {
		t.Errorf("dependabot.yml version = %d, want 2 (v1 is unsupported)", cfg.Version)
	}

	var patterns []string
	ok := false
	for _, u := range cfg.Updates {
		if u.Ecosystem != "github-actions" {
			continue
		}
		ok = true
		if dirs := u.dirs(); len(dirs) != 1 || dirs[0] != "/" {
			t.Errorf("the github-actions entry watches %v; workflows live in "+
				".github/workflows, which Dependabot finds via directory \"/\"", dirs)
		}
		for _, g := range u.Groups {
			patterns = append(patterns, g.Patterns...)
		}
	}
	if !ok {
		t.Fatal("dependabot.yml has no `github-actions` entry, so the SHA-pinned " +
			"actions in .github/workflows are never bumped")
	}

	for _, action := range workflowActions(t) {
		matched := false
		for _, p := range patterns {
			if globMatch(p, action) {
				matched = true
				break
			}
		}
		if !matched {
			t.Errorf("%s is not matched by any Dependabot group pattern %v, so it would "+
				"open its own PR outside the group (or be missed). Widen the pattern.",
				action, patterns)
		}
	}
}

// TestDependabotCoversEveryGoModule: nested modules pin their OWN deps, so a
// single "/" gomod entry silently leaves them unmanaged.
//
// This is a real, already-observed failure mode, not a hypothetical: bumping a dep
// in the root does not touch a nested module, and a stale nested go.mod surfaces
// as govulncheck failing with "updates to go.mod needed" — a message that names
// neither the module nor the cause (lagotto#43). Every go.mod in the tree must be
// listed, so adding a module without wiring it up fails here instead.
func TestDependabotCoversEveryGoModule(t *testing.T) {
	data, err := os.ReadFile(dotGithub("dependabot.yml"))
	if err != nil {
		t.Fatalf("read dependabot.yml: %v", err)
	}
	cfg, err := parseDependabot(data)
	if err != nil {
		t.Fatalf("dependabot.yml is not valid YAML: %v", err)
	}

	watched := map[string]bool{}
	for _, u := range cfg.Updates {
		if u.Ecosystem != "gomod" {
			continue
		}
		for _, d := range u.dirs() {
			watched[d] = true
		}
	}
	if len(watched) == 0 {
		t.Fatal("dependabot.yml has no `gomod` entry, so Go dependencies are never bumped")
	}

	mods := goModules(t)
	for _, m := range mods {
		if !watched[m] {
			t.Errorf("module %s is not watched by any gomod entry (watched: %v).\n"+
				"Nested modules pin their own deps, so this one's dependencies are "+
				"never updated and a stale go.mod there fails govulncheck with an "+
				"error that doesn't name it (lagotto#43).", m, keys(watched))
		}
	}
	// And the reverse: a listed directory with no module makes Dependabot error
	// every run ("no go.mod found"), which is noise that trains you to ignore it.
	real := map[string]bool{}
	for _, m := range mods {
		real[m] = true
	}
	for d := range watched {
		if !real[d] {
			t.Errorf("gomod entry watches %s but there is no go.mod there; "+
				"Dependabot errors on every run for a missing manifest", d)
		}
	}
}

type dependabotConfig struct {
	Version int                `yaml:"version"`
	Updates []dependabotUpdate `yaml:"updates"`
}

type dependabotUpdate struct {
	Ecosystem string `yaml:"package-ecosystem"`
	// Dependabot accepts either a single `directory:` or a `directories:` list;
	// dirs() normalizes the two so callers don't have to care which was used.
	Directory   string   `yaml:"directory"`
	Directories []string `yaml:"directories"`
	Groups      map[string]struct {
		Patterns []string `yaml:"patterns"`
	} `yaml:"groups"`
}

func (u dependabotUpdate) dirs() []string {
	if len(u.Directories) > 0 {
		return u.Directories
	}
	if u.Directory != "" {
		return []string{u.Directory}
	}
	return nil
}

func parseDependabot(data []byte) (dependabotConfig, error) {
	var cfg dependabotConfig
	err := yaml.Unmarshal(data, &cfg)
	return cfg, err
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// workflowActions returns the deduplicated owner/name of every registry action
// used by the workflows (local `./...` refs excluded — nothing to update).
func workflowActions(t *testing.T) []string {
	t.Helper()
	dir := dotGithub("workflows")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read workflows dir: %v", err)
	}
	seen := map[string]bool{}
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yml") && !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		for _, line := range strings.Split(string(data), "\n") {
			trimmed := strings.TrimPrefix(strings.TrimSpace(line), "- ")
			if !strings.HasPrefix(trimmed, "uses:") {
				continue
			}
			ref := strings.TrimSpace(strings.TrimPrefix(trimmed, "uses:"))
			if strings.HasPrefix(ref, "./") {
				continue
			}
			name, _, _ := strings.Cut(ref, "@")
			if !seen[name] {
				seen[name] = true
				out = append(out, name)
			}
		}
	}
	if len(out) == 0 {
		t.Fatal("found no actions in .github/workflows — this test would assert nothing; " +
			"check the parser against the current workflow layout")
	}
	sort.Strings(out)
	return out
}

// goModules returns every module directory in the repo as a Dependabot-style
// absolute path ("/" for the root, "/lambda/x" for a submodule).
func goModules(t *testing.T) []string {
	t.Helper()
	root := repoRoot()
	var out []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == "vendor" || d.Name() == ".git" || d.Name() == "node_modules") {
			return filepath.SkipDir
		}
		if d.IsDir() || d.Name() != "go.mod" {
			return nil
		}
		rel, err := filepath.Rel(root, filepath.Dir(path))
		if err != nil {
			return err
		}
		if rel == "." {
			out = append(out, "/")
		} else {
			out = append(out, "/"+filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk for go.mod: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("found no go.mod files — this test would assert nothing")
	}
	sort.Strings(out)
	return out
}

// globMatch implements the only wildcard Dependabot patterns use: `*`, matching
// any run of characters (including `/`, so `*` alone matches everything).
func globMatch(pattern, s string) bool {
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == s
	}
	if !strings.HasPrefix(s, parts[0]) {
		return false
	}
	s = s[len(parts[0]):]
	for _, p := range parts[1 : len(parts)-1] {
		i := strings.Index(s, p)
		if i < 0 {
			return false
		}
		s = s[i+len(p):]
	}
	return strings.HasSuffix(s, parts[len(parts)-1])
}
