package main

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const reviewSystemPrompt = `You are an expert code reviewer. You are reviewing a GitHub pull request diff.

# Review format
You MUST produce your review as inline comments embedded in the diff itself, similar to GitHub PR reviews. For each issue or observation, insert a review comment block directly after the relevant diff line(s) using this exact format:

` + "```" + `
+  some code line
>> [severity] comment text here
>> continuation of comment if needed
` + "```" + `

Where severity is one of:
- [critical] - bugs, security issues, data loss risks
- [warning] - potential issues, performance concerns, error handling gaps
- [suggestion] - style improvements, alternative approaches, minor cleanups
- [note] - observations, questions, or positive callouts

# Rules
- Only comment on changed lines (lines starting with + or -). Do not comment on unchanged context lines.
- Be specific and actionable. Reference concrete line content in your comments.
- Do not repeat the entire diff. Only output the sections of the diff where you have comments, with enough context (2-3 surrounding diff lines) to locate the comment.
- Group nearby comments when they relate to the same logical issue.
- After all inline comments, provide a brief summary section with:
  - Overall assessment (approve / request changes / comment)
  - Key themes (1-3 bullet points)

# Example output

` + "```" + `
--- a/server.go
+++ b/server.go
@@ -42,6 +42,8 @@
   func handleRequest(w http.ResponseWriter, r *http.Request) {
+    password := r.URL.Query().Get("pass")
>> [critical] Sensitive credentials should never be passed as query parameters.
>> They end up in server logs, browser history, and referrer headers.
>> Use a POST body or Authorization header instead.
+    db.Exec("SELECT * FROM users WHERE pw = " + password)
>> [critical] SQL injection vulnerability. Use parameterized queries:
>> db.Exec("SELECT * FROM users WHERE pw = $1", password)

--- a/utils.go
+++ b/utils.go
@@ -10,3 +10,5 @@
+func retry(fn func() error) {
+    for { fn() }
>> [warning] Infinite retry with no backoff, max attempts, or error logging.
>> This will spin-loop on persistent failures. Add exponential backoff and a limit.
+}

**Summary**
- Decision: **Request changes**
- SQL injection and credential exposure need fixing before merge
- Retry logic needs backoff and bounds
` + "```" + `

Be thorough but concise. Focus on substantive issues over style nitpicks.`

// reviewResult holds the outputs from an initial PR review so callers can
// decide whether to enter a follow-up REPL.
type reviewResult struct {
	Messages []Message
	Auth     *AuthMethod
	PRRef    string
}

func runReview(args []string) (*reviewResult, error) {
	prRef := ""
	if len(args) > 0 {
		prRef = args[0]
	}

	// Check that gh cli is available.
	if _, err := exec.LookPath("gh"); err != nil {
		return nil, fmt.Errorf("gh CLI not found. Install it from https://cli.github.com")
	}

	// Determine PR reference.
	if prRef == "" {
		// Try to detect from current branch.
		out, err := exec.Command("gh", "pr", "view", "--json", "number", "-q", ".number").Output()
		if err != nil {
			return nil, fmt.Errorf("no PR specified and could not detect one for the current branch.\nUsage: oc review [pr-number|pr-url|branch]")
		}
		prRef = strings.TrimSpace(string(out))
		if prRef == "" {
			return nil, fmt.Errorf("no PR found for the current branch")
		}
	}

	fmt.Fprintf(os.Stderr, "Fetching PR %s...\n", prRef)

	// Fetch PR metadata.
	metaOut, err := exec.Command("gh", "pr", "view", prRef, "--json", "number,title,author,baseRefName,headRefName,additions,deletions,changedFiles").Output()
	if err != nil {
		return nil, fmt.Errorf("failed to fetch PR metadata: %w", err)
	}

	// Fetch PR diff.
	diffOut, err := exec.Command("gh", "pr", "diff", prRef).Output()
	if err != nil {
		return nil, fmt.Errorf("failed to fetch PR diff: %w", err)
	}
	diff := string(diffOut)
	if strings.TrimSpace(diff) == "" {
		return nil, fmt.Errorf("PR diff is empty")
	}

	// Truncate very large diffs to fit context window.
	maxDiffChars := 80000
	truncated := false
	if len(diff) > maxDiffChars {
		diff = diff[:maxDiffChars]
		truncated = true
	}

	// Build the review prompt.
	var prompt strings.Builder
	prompt.WriteString("PR Metadata:\n")
	prompt.WriteString(strings.TrimSpace(string(metaOut)))
	prompt.WriteString("\n\n")
	if truncated {
		prompt.WriteString("NOTE: The diff was truncated due to size. Review what is shown.\n\n")
	}
	prompt.WriteString("Diff:\n```diff\n")
	prompt.WriteString(diff)
	prompt.WriteString("\n```\n\nReview this pull request.")

	auth, err := getAuth()
	if err != nil {
		return nil, fmt.Errorf("auth error: %w", err)
	}
	if auth == nil {
		return nil, fmt.Errorf("no authentication found. Use `oc login` first")
	}

	fmt.Fprintf(os.Stderr, "Reviewing...\n")
	start := time.Now()

	messages := []Message{
		{Role: "user", Content: prompt.String()},
	}

	review, err := compactionChat(messages, auth, reviewSystemPrompt)
	if err != nil {
		return nil, fmt.Errorf("review failed: %w", err)
	}

	messages = append(messages, Message{Role: "assistant", Content: review})

	elapsed := time.Since(start)
	fmt.Println()
	fmt.Println(renderReview(review))
	fmt.Println()
	fmt.Fprintln(os.Stderr, dim(fmt.Sprintf("review completed in %s", elapsed.Round(time.Millisecond))))

	return &reviewResult{Messages: messages, Auth: auth, PRRef: prRef}, nil
}

// reviewREPL enters an interactive follow-up loop after a PR review.
func reviewREPL(res *reviewResult) {
	messages := res.Messages

	fmt.Println(dim("Ask follow-up questions about this PR, or press Ctrl+D / type /quit to exit."))

	for {
		fmt.Print("review> ")
		input, err := readLine()
		if err != nil {
			fmt.Println()
			return
		}

		input = strings.TrimSpace(input)
		if input == "" {
			continue
		}
		if input == "/quit" || input == "/exit" || input == "/q" {
			return
		}

		messages = append(messages, Message{Role: "user", Content: input})

		stats, err := handleResponse(&messages, res.Auth, input, "", nil, nil)
		if err != nil {
			if err.Error() == "interrupted" {
				// Remove the user message so they can retype.
				if len(messages) > 0 && messages[len(messages)-1].Role == "user" {
					messages = messages[:len(messages)-1]
				}
				fmt.Println(dim("Request cancelled. Your message was discarded -- retype to try again."))
				continue
			}
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
		}
		_ = stats
	}
}

// renderReview formats the review output with colors for inline comments.
func renderReview(review string) string {
	lines := strings.Split(review, "\n")
	var b strings.Builder

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		// Inline review comments: lines starting with >>
		if strings.HasPrefix(trimmed, ">>") {
			comment := strings.TrimPrefix(trimmed, ">>")
			comment = strings.TrimSpace(comment)

			// Color by severity tag.
			color := "\033[33m" // default yellow
			if strings.HasPrefix(comment, "[critical]") {
				color = "\033[1;31m" // bold red
			} else if strings.HasPrefix(comment, "[warning]") {
				color = "\033[33m" // yellow
			} else if strings.HasPrefix(comment, "[suggestion]") {
				color = "\033[36m" // cyan
			} else if strings.HasPrefix(comment, "[note]") {
				color = "\033[32m" // green
			}

			b.WriteString("  ")
			b.WriteString(color)
			b.WriteString(">> ")
			b.WriteString(comment)
			b.WriteString("\033[0m")
			b.WriteByte('\n')
			continue
		}

		// Diff lines within the review output.
		if strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++") {
			b.WriteString("  \033[32m")
			b.WriteString(line)
			b.WriteString("\033[0m\n")
		} else if strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---") {
			b.WriteString("  \033[31m")
			b.WriteString(line)
			b.WriteString("\033[0m\n")
		} else if strings.HasPrefix(line, "@@") {
			b.WriteString("  \033[36;2m")
			b.WriteString(line)
			b.WriteString("\033[0m\n")
		} else if strings.HasPrefix(line, "---") || strings.HasPrefix(line, "+++") {
			b.WriteString("  \033[1m")
			b.WriteString(line)
			b.WriteString("\033[0m\n")
		} else if strings.HasPrefix(trimmed, "**") || strings.HasPrefix(trimmed, "- Decision:") || strings.HasPrefix(trimmed, "- ") {
			// Summary lines: render with markdown.
			b.WriteString(renderMarkdown(line))
			b.WriteByte('\n')
		} else {
			b.WriteString("  \033[2m")
			b.WriteString(line)
			b.WriteString("\033[0m\n")
		}
	}

	return strings.TrimRight(b.String(), "\n")
}

// parseReviewPRNumber extracts a PR number from user input like "/review 123" or "/review".
func parseReviewPRNumber(input string) string {
	parts := strings.Fields(input)
	if len(parts) < 2 {
		return ""
	}
	// Could be a number, URL, or branch name -- pass through to gh.
	return parts[1]
}

// runReviewInteractive is the /review handler for interactive mode.
func runReviewInteractive(input string) {
	ref := parseReviewPRNumber(input)
	var args []string
	if ref != "" {
		args = []string{ref}
	}
	_, err := runReview(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "review error: %v\n", err)
	}
	// In interactive mode the user is already in a REPL, so don't
	// start a nested one -- just return to the main prompt.
}

// runReviewCommand is the `oc review` subcommand handler.
func runReviewCommand(args []string) {
	// Parse flags.
	var prRef string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if strings.HasPrefix(arg, "-") {
			// Skip unknown flags.
			continue
		}
		if prRef == "" {
			prRef = arg
		}
	}

	// Validate numeric PR if given.
	if prRef != "" {
		if _, err := strconv.Atoi(prRef); err != nil {
			// Not a number -- could be a URL or branch, pass through.
		}
	}

	var reviewArgs []string
	if prRef != "" {
		reviewArgs = []string{prRef}
	}
	res, err := runReview(reviewArgs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	reviewREPL(res)
}
