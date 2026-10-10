// mock-agent emulates a CLI coding agent running an SDD framework command.
// It is deterministic and used by local development, CI, and the vertical
// slice. Behavior can be steered with tags in any string input:
//
//	[mock:no-hitl]        complete without asking a question
//	[mock:approval]       ask for an approval instead of a multiple-choice question
//	[mock:fail]           exit with a failure result
//	[mock:skip-artifact]  "succeed" without producing required artifacts
//	[mock:slow]           sleep between steps (useful for cancellation tests)
//	[mock:secret-leak]    print a fake token to verify log redaction
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const exitAwaitingInput = 75

type invocation struct {
	Command           string         `json:"command"`
	Inputs            map[string]any `json:"inputs"`
	ExpectedArtifacts []string       `json:"expectedArtifacts"`
	Repositories      []struct {
		RepositoryID   string `json:"repositoryId"`
		URL            string `json:"url"`
		ResolvedCommit string `json:"resolvedCommit"`
		CheckoutPath   string `json:"checkoutPath"`
	} `json:"repositories"`
}

type answer struct {
	AgentRequestID string `json:"agentRequestId"`
	Response       struct {
		Decision string   `json:"decision"`
		Values   []string `json:"values"`
		Text     string   `json:"text"`
		Comment  string   `json:"comment"`
	} `json:"response"`
}

type checkpoint struct {
	Session   string    `json:"session"`
	Command   string    `json:"command"`
	Pending   string    `json:"pendingRequestId"`
	StepsDone []string  `json:"stepsDone"`
	SavedAt   time.Time `json:"savedAt"`
}

func emit(v map[string]any) {
	b, _ := json.Marshal(v)
	fmt.Println(string(b))
}

func logf(format string, args ...any) {
	emit(map[string]any{"type": "log", "level": "info", "message": fmt.Sprintf(format, args...)})
}

func main() {
	if len(os.Args) < 2 || os.Args[1] != "run" {
		fmt.Fprintln(os.Stderr, "usage: mock-agent run --invocation FILE --output-dir DIR --checkpoint-dir DIR --mode live|checkpoint [--resume-answer FILE]")
		os.Exit(64)
	}
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	invPath := fs.String("invocation", "", "")
	workdir := fs.String("workdir", ".", "")
	outDir := fs.String("output-dir", "", "")
	cpDir := fs.String("checkpoint-dir", "", "")
	mode := fs.String("mode", "checkpoint", "")
	answerPath := fs.String("resume-answer", "", "")
	_ = fs.Parse(os.Args[2:])

	var inv invocation
	b, err := os.ReadFile(*invPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "read invocation:", err)
		os.Exit(2)
	}
	if err := json.Unmarshal(b, &inv); err != nil {
		fmt.Fprintln(os.Stderr, "parse invocation:", err)
		os.Exit(2)
	}
	// Tags may appear in any string input (commands define their own inputs).
	var parts []string
	for _, v := range inv.Inputs {
		if sv, ok := v.(string); ok {
			parts = append(parts, sv)
		}
	}
	instr := strings.Join(parts, " ")
	has := func(tag string) bool { return strings.Contains(instr, "[mock:"+tag+"]") }
	pause := func() {
		if has("slow") {
			time.Sleep(3 * time.Second)
		} else {
			time.Sleep(150 * time.Millisecond)
		}
	}

	cp := checkpoint{Session: fmt.Sprintf("mock-%d", time.Now().UnixNano()), Command: inv.Command}
	var ans *answer
	if *answerPath != "" {
		raw, err := os.ReadFile(filepath.Join(*cpDir, "state.json"))
		if err != nil {
			fmt.Fprintln(os.Stderr, "resume requested but no checkpoint found:", err)
			os.Exit(3)
		}
		if err := json.Unmarshal(raw, &cp); err != nil {
			fmt.Fprintln(os.Stderr, "corrupt checkpoint:", err)
			os.Exit(3)
		}
		ans = &answer{}
		raw, err = os.ReadFile(*answerPath)
		if err != nil || json.Unmarshal(raw, ans) != nil {
			fmt.Fprintln(os.Stderr, "cannot read answer")
			os.Exit(3)
		}
		if ans.AgentRequestID != cp.Pending {
			fmt.Fprintf(os.Stderr, "answer is for %q but checkpoint awaits %q\n", ans.AgentRequestID, cp.Pending)
			os.Exit(3)
		}
		logf("resumed session %s from checkpoint saved at %s", cp.Session, cp.SavedAt.Format(time.RFC3339))
	}

	logf("mock-agent executing %s in %s (mode=%s)", inv.Command, *workdir, *mode)
	if has("secret-leak") {
		// Deliberately print something that looks like a credential.
		logf("debug: using token ghp_abcdefghijklmnopqrstuvwxyz0123456789 for git")
	}
	steps := []string{"load-framework", "analyze-context", "draft"}
	for _, s := range steps {
		if contains(cp.StepsDone, s) {
			continue
		}
		emit(map[string]any{"type": "progress", "step": s})
		pause()
		cp.StepsDone = append(cp.StepsDone, s)
	}
	for _, r := range inv.Repositories {
		logf("repository %s checked out at %s (%s)", r.RepositoryID, r.CheckoutPath, short(r.ResolvedCommit))
	}

	// Human-in-the-loop.
	if !has("no-hitl") && ans == nil {
		reqID := "q-1"
		req := map[string]any{"type": "interaction", "id": reqID, "step": "clarify"}
		if has("approval") {
			req["kind"] = "APPROVAL"
			req["prompt"] = fmt.Sprintf("The draft for %s is ready. Approve it for finalization?", inv.Command)
			req["schema"] = map[string]any{"required": false}
		} else {
			req["kind"] = "MULTIPLE_CHOICE"
			req["prompt"] = fmt.Sprintf("%s: which level of detail should the output target?", inv.Command)
			req["options"] = []map[string]string{
				{"value": "concise", "label": "Concise", "description": "Key points only"},
				{"value": "standard", "label": "Standard", "description": "Balanced detail"},
				{"value": "exhaustive", "label": "Exhaustive", "description": "Every edge case"},
			}
			req["schema"] = map[string]any{"required": true}
		}
		emit(req)
		cp.Pending = reqID
		if *mode == "live" {
			logf("waiting for response on stdin (live session)")
			line, err := bufio.NewReader(os.Stdin).ReadString('\n')
			if err != nil {
				fmt.Fprintln(os.Stderr, "stdin closed while waiting for input")
				os.Exit(4)
			}
			ans = &answer{AgentRequestID: reqID}
			if err := json.Unmarshal([]byte(line), &ans.Response); err != nil {
				fmt.Fprintln(os.Stderr, "malformed response")
				os.Exit(4)
			}
		} else {
			cp.SavedAt = time.Now().UTC()
			raw, _ := json.MarshalIndent(cp, "", "  ")
			if err := os.WriteFile(filepath.Join(*cpDir, "state.json"), raw, 0o600); err != nil {
				fmt.Fprintln(os.Stderr, "write checkpoint:", err)
				os.Exit(5)
			}
			emit(map[string]any{"type": "checkpoint", "message": "session checkpointed; awaiting input"})
			os.Exit(exitAwaitingInput)
		}
	}

	detail := "standard"
	if ans != nil {
		logf("received human response: decision=%s values=%v", ans.Response.Decision, ans.Response.Values)
		if ans.Response.Decision == "REJECT" {
			emit(map[string]any{"type": "result", "status": "failure", "summary": "Draft rejected by reviewer: " + ans.Response.Comment})
			os.Exit(2)
		}
		if len(ans.Response.Values) > 0 {
			detail = ans.Response.Values[0]
		}
	}

	emit(map[string]any{"type": "progress", "step": "finalize"})
	pause()
	if has("fail") {
		fmt.Fprintln(os.Stderr, "simulated agent failure")
		emit(map[string]any{"type": "result", "status": "failure", "summary": "Simulated failure requested by [mock:fail]"})
		os.Exit(1)
	}
	if !has("skip-artifact") {
		for _, t := range inv.ExpectedArtifacts {
			if t == "diff" && len(inv.Repositories) > 0 && inv.Repositories[0].CheckoutPath != "" {
				// Make a real change; the supervisor derives the diff artifact
				// from the repository itself.
				r := inv.Repositories[0]
				note := filepath.Join(r.CheckoutPath, "ASTRION_MOCK_CHANGE.md")
				body := fmt.Sprintf("# Mock change\n\nImplemented by mock-agent for `%s` (%s detail).\n", inv.Command, detail)
				if err := os.WriteFile(note, []byte(body), 0o644); err != nil {
					fmt.Fprintln(os.Stderr, "modify repository:", err)
					os.Exit(5)
				}
				logf("modified repository %s", r.RepositoryID)
				continue
			}
			name, ct, body := render(t, inv, detail, instr)
			if err := os.WriteFile(filepath.Join(*outDir, name), []byte(body), 0o644); err != nil {
				fmt.Fprintln(os.Stderr, "write artifact:", err)
				os.Exit(5)
			}
			emit(map[string]any{"type": "artifact", "path": name, "artifactType": t, "contentType": ct})
		}
	}
	emit(map[string]any{"type": "result", "status": "success", "summary": fmt.Sprintf("%s completed (%s detail)", inv.Command, detail)})
}

func render(artifactType string, inv invocation, detail, instr string) (name, contentType, body string) {
	keys := make([]string, 0, len(inv.Inputs))
	for k := range inv.Inputs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var in strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&in, "- **%s**: %v\n", k, inv.Inputs[k])
	}
	switch artifactType {
	case "diff":
		return "changes.diff", "text/x-diff", "diff --git a/README.md b/README.md\n--- a/README.md\n+++ b/README.md\n@@ -1 +1,2 @@\n # Project\n+Implemented by mock-agent for " + inv.Command + "\n"
	case "test-report":
		return "test-report.json", "application/json", `{"tool":"mock","passed":12,"failed":0,"skipped":1}` + "\n"
	case "jira-links":
		return "jira-links.json", "application/json", `{"issues":[{"key":"MOCK-1","summary":"Generated by mock-agent (no real Jira call)"}]}` + "\n"
	}
	title := strings.ReplaceAll(artifactType, "-", " ")
	body = fmt.Sprintf("# %s\n\nGenerated by **mock-agent** for `%s` at %s detail.\n\n## Inputs\n\n%s\n## Notes\n\nThis is deterministic placeholder content produced by the mock SDD framework.\n",
		strings.ToUpper(title[:1])+title[1:], inv.Command, detail, in.String())
	return artifactType + ".md", "text/markdown", body
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	if s == "" {
		return "unknown"
	}
	return s
}
