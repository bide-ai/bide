package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// A pattern is one removed name, as it can appear in code or prose, and what replaced it.
type pattern struct {
	re  *regexp.Regexp
	use string
	sub bool // the name is the first submatch (word's), not the whole match
}

// word matches name as an identifier or a part of one that starts with it (a test named
// TestRunSaga_X, a helper runResultOf is not: lower case), and not inside a longer word
// (SendMessages).
func word(name, use string) pattern {
	return pattern{regexp.MustCompile(`(?:^|[^A-Za-z0-9]|Test|Example|Benchmark|Fuzz)(` + name + `)(?:$|[^a-z0-9])`), use, true}
}

// patterns are the removed names. A name that is also an English word or a name another package
// may use (Durable, Build, Interrupted, Final, Send) is matched only in the forms the old API
// used it in.
var patterns = []pattern{
	{regexp.MustCompile(`\bagent\.Durable\b|\bDurable\.(Do|History)\b|\bDurable interface\b`), "*agent.Journal over an agent.Store", false},
	word("durabletest", "storetest"),
	word("CheckDurableWrapper", "a Store wrapper's Unwrap() Store"),
	word("RunSagaResult", "Run(..., WithSaga())"),
	word("RunSaga", "Run(..., WithSaga())"),
	word("StreamSaga", "Stream(..., WithSaga())"),
	word("RunResult", "Run"),
	word("RunMessage", "Run"),
	word("StreamMessage", "Stream"),
	word("ResumeRun", "Resume"),
	word("RunTypedMessage", "RunTyped"),
	word("RunTypedNative", "RunTyped with WithOutputMode(OutputNative)"),
	word("SendMessageOnce", "Session.SendOnce"),
	word("SendMessage", "Session.Send"),
	word("AgentStream", "RunStream"),
	word("AgentEvent", "RunEvent"),
	word("ResolveHaltRef", "ResolveHalt"),
	word("ResolveStepHalt", "ResolveHalt"),
	word("PendingApproval", "ApprovalPending"),
	word("ResumeHalt", "OutcomeUnknown"),
	{regexp.MustCompile(`(\*|\bagent\.)Interrupted\b`), "InterruptPending", false},
	{regexp.MustCompile(`(\*|\bagent\.)Awaiting\b`), "SignalPending", false},
	{regexp.MustCompile(`(\*|\bagent\.)Sleeping\b`), "TimerPending", false},
	word("ApproveAs", "SubmitDecision"),
	word("ContextWithIdentity", "the WithIdentity run option"),
	word("ContextWithWaker", "the WithWaker run option"),
	word("ContextWithClock", "the WithClock run option"),
	word("SetMaxConcurrency", "WithMaxConcurrency"),
	word("UseTool", "WithToolMiddleware"),
	word("SpecOf", "Tool.Spec"),
	{regexp.MustCompile(`\bagent\.Build\b`), "agent.New", false},
	{regexp.MustCompile(`\bagent\.Send\b`), "Journal.Enqueue", false},
	{regexp.MustCompile(`\bagent\.(New)?ScriptedModel\b`), "agenttest.ScriptedModel", false},
	{regexp.MustCompile(`\.Final\(\)`), "RunStream.Result", false},
	{regexp.MustCompile(`\bagent\.(Step|Parallel|Signal|Enqueue|AnswerInterrupt|Resume)\b`), "the Journal method (j.Step, j.Parallel, j.Signal, j.Enqueue, j.AnswerInterrupt)", false},
	{regexp.MustCompile(`\bagent\.(TextTurn|ToolTurn|ErrorTurn|ScriptedTurn)\b`), "agenttest's", false},
	word("RunDurable", "storetest.Run"),
	{regexp.MustCompile(`bide-ai/bide/mcp([^a-z]|$)`), "github.com/bide-ai/bide/mcptools", false},
}

// transitional is checked in Go code and godoc only: prose elsewhere may use the word.
var transitional = pattern{regexp.MustCompile(`(?i)\btransitional\b`), "remove the comment, or name the current API", false}

// excluded are the paths (relative to the root, slash-separated) whose files may name the
// removed API: the migrate tool, this command, and the historical records.
var excluded = []string{
	"internal/tools/migrate/",
	"internal/tools/oldnames/",
	"CHANGELOG.md",
	"docs/releases/",
	"docs/design/",
}

func isExcluded(rel string) bool {
	for _, e := range excluded {
		if rel == e || (strings.HasSuffix(e, "/") && strings.HasPrefix(rel, e)) {
			return true
		}
	}
	return false
}

// CheckTree scans every .go, go.mod and .md file below root, except the excluded paths, and returns one
// finding per match, as "path:line: Name: use X".
func CheckTree(root string) ([]string, error) {
	var findings []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if n := d.Name(); rel != "." && (strings.HasPrefix(n, ".") || n == "node_modules" || isExcluded(rel+"/")) {
				return filepath.SkipDir
			}
			return nil
		}
		isGo := strings.HasSuffix(rel, ".go") || filepath.Base(rel) == "go.mod"
		if (!isGo && !strings.HasSuffix(rel, ".md")) || isExcluded(rel) {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		findings = append(findings, check(rel, data, isGo)...)
		return nil
	})
	return findings, err
}

func check(name string, data []byte, withTransitional bool) []string {
	var findings []string
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for n := 1; sc.Scan(); n++ {
		line := sc.Text()
		ps := patterns
		if withTransitional {
			ps = append(ps[:len(ps):len(ps)], transitional)
		}
		var taken [][2]int     // the spans reported on this line: a longer name listed first wins
		for _, p := range ps { // one finding per name and line, at its first match
			m := p.re.FindStringSubmatchIndex(line)
			if m == nil {
				continue
			}
			at := [2]int{m[0], m[1]}
			if p.sub {
				at = [2]int{m[2], m[3]}
			}
			if slices.ContainsFunc(taken, func(t [2]int) bool { return at[0] < t[1] && t[0] < at[1] }) {
				continue
			}
			taken = append(taken, at)
			findings = append(findings, fmt.Sprintf("%s:%d: %s: use %s", name, n, strings.TrimSpace(line[at[0]:at[1]]), p.use))
		}
	}
	return findings
}

// CheckGodoc runs `go doc -all` on every non-internal package of every module below root (each
// directory with a go.mod, outside testdata and the excluded paths) and checks its output.
func CheckGodoc(root string) ([]string, error) {
	var mods []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if n := d.Name(); rel != "." && (strings.HasPrefix(n, ".") || n == "testdata" || n == "node_modules" || isExcluded(rel+"/")) {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() == "go.mod" {
			mods = append(mods, filepath.Dir(path))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	var findings []string
	for _, dir := range mods {
		cmd := exec.Command("go", "list", "./...")
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GOWORK=off")
		out, err := cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("go list in %s: %w", dir, err)
		}
		for _, pkg := range strings.Fields(string(out)) {
			if strings.Contains(pkg+"/", "/internal/") {
				continue
			}
			cmd := exec.Command("go", "doc", "-all", pkg)
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "GOWORK=off")
			doc, err := cmd.Output()
			if err != nil {
				return nil, fmt.Errorf("go doc -all %s: %w", pkg, err)
			}
			findings = append(findings, check("go doc -all "+pkg, doc, true)...)
		}
	}
	return findings, nil
}
