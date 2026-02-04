package main

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

var (
	tmuxPath   string
	cccPath    string
	claudePath string

	// Per-session mutex to ensure messages are sent one at a time
	sessionMutexes   = make(map[string]*sync.Mutex)
	sessionMutexLock sync.Mutex
)

// getSessionMutex returns a mutex for the given session name
func getSessionMutex(session string) *sync.Mutex {
	sessionMutexLock.Lock()
	defer sessionMutexLock.Unlock()
	if _, ok := sessionMutexes[session]; !ok {
		sessionMutexes[session] = &sync.Mutex{}
	}
	return sessionMutexes[session]
}

func initPaths() {
	// Find tmux binary
	if path, err := exec.LookPath("tmux"); err == nil {
		tmuxPath = path
	} else {
		// Fallback paths for common installations
		for _, p := range []string{"/opt/homebrew/bin/tmux", "/usr/local/bin/tmux", "/usr/bin/tmux"} {
			if _, err := os.Stat(p); err == nil {
				tmuxPath = p
				break
			}
		}
	}

	// Find ccc binary - prefer ~/bin/ccc (canonical install path),
	// then PATH, then current executable as last resort
	home, _ := os.UserHomeDir()
	binCcc := home + "/bin/ccc"
	if _, err := os.Stat(binCcc); err == nil {
		cccPath = binCcc
	} else if path, err := exec.LookPath("ccc"); err == nil {
		cccPath = path
	} else if exe, err := os.Executable(); err == nil {
		cccPath = exe
	}

	// Find claude binary - first try PATH, then fallback paths
	if path, err := exec.LookPath("claude"); err == nil {
		claudePath = path
	} else {
		home, _ := os.UserHomeDir()
		claudePaths := []string{
			home + "/.local/bin/claude",
			"/usr/local/bin/claude",
		}
		for _, p := range claudePaths {
			if _, err := os.Stat(p); err == nil {
				claudePath = p
				break
			}
		}
	}
}

func tmuxSessionExists(name string) bool {
	cmd := exec.Command(tmuxPath, "has-session", "-t", name)
	return cmd.Run() == nil
}

func createTmuxSession(name string, workDir string, continueSession bool) error {
	// Build the command to run inside tmux
	cccCmd := cccPath + " run"
	if continueSession {
		cccCmd += " -c"
	}

	// Ensure tmux server has fast escape-time (critical for reliable key delivery)
	exec.Command(tmuxPath, "set-option", "-s", "escape-time", "10").Run()

	// Create tmux session with a login shell (don't run command directly - it kills session on exit)
	args := []string{"new-session", "-d", "-s", name, "-c", workDir}
	cmd := exec.Command(tmuxPath, args...)
	if err := cmd.Run(); err != nil {
		return err
	}

	// Enable mouse mode for this session (allows scrolling)
	exec.Command(tmuxPath, "set-option", "-t", name, "mouse", "on").Run()

	// Send the command to the session via send-keys (preserves TTY properly)
	time.Sleep(200 * time.Millisecond)
	exec.Command(tmuxPath, "send-keys", "-t", name, cccCmd, "C-m").Run()

	return nil
}

// runClaudeRaw runs claude directly (used inside tmux sessions)
func runClaudeRaw(continueSession bool) error {
	if claudePath == "" {
		return fmt.Errorf("claude binary not found")
	}

	args := []string{"--dangerously-skip-permissions"}
	if continueSession {
		args = append(args, "-c")
	}

	cmd := exec.Command(claudePath, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	// Ensure OAuth token is available from config if not already in environment
	if os.Getenv("CLAUDE_CODE_OAUTH_TOKEN") == "" {
		if config, err := loadConfig(); err == nil && config.OAuthToken != "" {
			cmd.Env = append(os.Environ(), "CLAUDE_CODE_OAUTH_TOKEN="+config.OAuthToken)
		}
	}

	return cmd.Run()
}

// waitForClaude polls the tmux pane until Claude Code's input prompt appears
func waitForClaude(session string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		cmd := exec.Command(tmuxPath, "capture-pane", "-t", session, "-p")
		out, err := cmd.Output()
		if err == nil {
			content := string(out)
			// Claude Code shows "❯" when ready for input
			if strings.Contains(content, "❯") {
				return nil
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("timeout waiting for Claude to start")
}

func sendToTmux(session string, text string) error {
	// Acquire per-session lock to ensure messages are sent sequentially
	mu := getSessionMutex(session)
	mu.Lock()
	defer mu.Unlock()

	// Calculate delay based on text length
	// Base: 50ms + 0.5ms per character, capped at 5 seconds
	baseDelay := 50 * time.Millisecond
	charDelay := time.Duration(len(text)) * 500 * time.Microsecond // 0.5ms per char
	delay := baseDelay + charDelay
	if delay > 5*time.Second {
		delay = 5 * time.Second
	}
	return sendToTmuxWithDelay(session, text, delay)
}

func sendToTmuxWithDelay(session string, text string, delay time.Duration) error {
	// Ensure tmux has fast escape-time (critical for reliable key delivery)
	exec.Command(tmuxPath, "set-option", "-s", "escape-time", "10").Run()

	// CRITICAL: Wait for Claude to be idle at the prompt BEFORE sending text.
	// If we send while Claude is processing, text goes into the buffer but doesn't
	// get submitted until Claude finishes, causing "queued messages" behavior.
	maxWaitForIdle := 5 * time.Minute
	pollInterval := 500 * time.Millisecond
	startWait := time.Now()

	for {
		state := captureSessionState(session)

		// Ready to send: Claude is at the prompt and not processing
		if state.hasPrompt && !state.isProcessing {
			break
		}

		// Timeout - send anyway and hope for the best
		if time.Since(startWait) > maxWaitForIdle {
			fmt.Printf("[ccc] ⚠ Timeout waiting for Claude idle, sending anyway\n")
			break
		}

		// Still processing - wait and check again
		if state.isProcessing {
			fmt.Printf("[ccc] Claude is processing, waiting to send message...\n")
		}
		time.Sleep(pollInterval)
	}

	// Now send the text
	cmd := exec.Command(tmuxPath, "send-keys", "-t", session, "-l", text)
	if err := cmd.Run(); err != nil {
		return err
	}

	// Wait for content to load (e.g., images)
	time.Sleep(delay)

	// Send Enter twice (Claude Code needs double Enter to submit)
	exec.Command(tmuxPath, "send-keys", "-t", session, "C-m").Run()
	time.Sleep(30 * time.Millisecond)
	exec.Command(tmuxPath, "send-keys", "-t", session, "C-m").Run()

	// Verify delivery: check that Claude received the message
	maxRetries := 5
	for retry := 0; retry < maxRetries; retry++ {
		time.Sleep(200 * time.Millisecond)

		state := captureSessionState(session)

		// SUCCESS: Claude is processing (our message was submitted)
		if state.isProcessing {
			fmt.Printf("[ccc] ✓ Message delivered (Claude processing)\n")
			return nil
		}

		// SUCCESS: Text no longer in input buffer (was submitted)
		textPrefix := firstNChars(text, 20)
		if !strings.Contains(state.inputContent, textPrefix) {
			fmt.Printf("[ccc] ✓ Message delivered (input cleared)\n")
			return nil
		}

		// Text still in buffer — send Enter again
		fmt.Printf("[ccc] Retry %d: text still in buffer, sending Enter\n", retry+1)
		exec.Command(tmuxPath, "send-keys", "-t", session, "C-m").Run()
		time.Sleep(30 * time.Millisecond)
		exec.Command(tmuxPath, "send-keys", "-t", session, "C-m").Run()
	}

	// Final check
	finalState := captureSessionState(session)
	if finalState.isProcessing || !strings.Contains(finalState.inputContent, firstNChars(text, 20)) {
		fmt.Printf("[ccc] ✓ Message delivered (final check)\n")
		return nil
	}

	fmt.Printf("[ccc] ⚠ Message delivery uncertain\n")
	return nil
}

// sessionState captures the current state of a Claude Code tmux session
type sessionState struct {
	isProcessing  bool   // Claude is actively working (spinner visible)
	hasPrompt     bool   // Input prompt ❯ is visible
	promptLineNum int    // Line number of the prompt
	inputContent  string // Content in the input area (after ❯)
	contentHash   string // Hash of visible content (to detect changes)
}

func captureSessionState(session string) sessionState {
	out, err := exec.Command(tmuxPath, "capture-pane", "-t", session, "-p").Output()
	if err != nil {
		return sessionState{}
	}

	content := string(out)
	lines := strings.Split(content, "\n")

	state := sessionState{
		contentHash: fmt.Sprintf("%d", len(content)), // Simple hash: length
	}

	// Check for processing indicators
	state.isProcessing = isClaudeProcessing(content)

	// Find prompt and extract input content
	for i := len(lines) - 1; i >= 0; i-- {
		line := lines[i]
		if strings.Contains(line, "❯") {
			state.hasPrompt = true
			state.promptLineNum = i

			// Extract content after prompt
			promptIdx := strings.LastIndex(line, "❯")
			afterPrompt := strings.TrimSpace(line[promptIdx+len("❯"):])

			// Also get content from lines below (multi-line input)
			var inputLines []string
			if afterPrompt != "" {
				inputLines = append(inputLines, afterPrompt)
			}
			for j := i + 1; j < len(lines) && j < i+10; j++ {
				nextLine := strings.TrimSpace(lines[j])
				// Stop at status/decoration lines
				if strings.HasPrefix(nextLine, "[") || strings.HasPrefix(nextLine, "───") ||
					strings.HasPrefix(nextLine, "Press") || nextLine == "" {
					break
				}
				inputLines = append(inputLines, nextLine)
			}
			state.inputContent = strings.Join(inputLines, " ")
			break
		}
	}

	return state
}

func firstNChars(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// isClaudeProcessing checks if Claude Code is actively processing (not idle at prompt).
func isClaudeProcessing(paneContent string) bool {
	lines := strings.Split(paneContent, "\n")

	// Check each line for spinners at the START of the line (after trimming)
	// This avoids false positives from ● bullets in tool output
	// Skip completion indicators like "✻ Brewed for 2m 52s" - these use many verbs
	spinners := []string{"✻", "◐", "◑", "◒", "◓", "✢", "✽", "·", "⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		// Completion indicators match pattern "✻ <Verb>ed for Xm Ys" or "✻ <Verb>ed for Xs"
		// Skip any line containing " for " followed by duration (e.g., "for 2m", "for 35s")
		if strings.HasPrefix(trimmed, "✻") && strings.Contains(trimmed, " for ") {
			continue
		}
		for _, s := range spinners {
			if strings.HasPrefix(trimmed, s) {
				return true
			}
		}
		// Also check for status line spinner (e.g., "◐ Bash:")
		if strings.Contains(line, "◐ ") || strings.Contains(line, "◑ ") ||
			strings.Contains(line, "◒ ") || strings.Contains(line, "◓ ") {
			return true
		}
	}

	// Activity indicators that appear in thinking/status lines
	activityIndicators := []string{
		"Thinking…", "Cooking…", "Pondering…", "Flowing…", "Crunching…",
		"Churning…", "Sublimating…", "thinking)", "thought for",
		"Running PreToolUse", "Running PostToolUse", "Running…",
		"(timeout", "· timeout", "Waiting for task",
	}
	for _, indicator := range activityIndicators {
		if strings.Contains(paneContent, indicator) {
			return true
		}
	}

	return false
}

// isTextStuckInBuffer checks if the sent text is still sitting in the input
// buffer (visible after the ❯ prompt) rather than having been submitted.
func isTextStuckInBuffer(paneContent string, sentText string) bool {
	lines := strings.Split(paneContent, "\n")

	// Find the last line containing the prompt marker
	promptLineIdx := -1
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.Contains(lines[i], "❯") {
			promptLineIdx = i
			break
		}
	}
	if promptLineIdx == -1 {
		return false // no prompt visible, can't tell
	}

	// Get content after the prompt marker on the prompt line
	promptLine := lines[promptLineIdx]
	promptIdx := strings.LastIndex(promptLine, "❯")
	afterPromptOnLine := strings.TrimSpace(promptLine[promptIdx+len("❯"):])

	// Also check the lines immediately following the prompt (multi-line input)
	var inputAreaContent string
	inputAreaContent = afterPromptOnLine
	for i := promptLineIdx + 1; i < len(lines) && i < promptLineIdx+10; i++ {
		line := strings.TrimSpace(lines[i])
		// Stop at status line indicators (Claude HUD, etc.)
		if strings.HasPrefix(line, "[") || strings.HasPrefix(line, "───") {
			break
		}
		inputAreaContent += " " + line
	}
	inputAreaContent = strings.TrimSpace(inputAreaContent)

	// If input area has content, text is stuck
	if len(inputAreaContent) > 0 {
		// Double check it's actually our text (not just stray characters)
		check := sentText
		if idx := strings.IndexByte(check, '\n'); idx > 0 {
			check = check[:idx]
		}
		if len(check) > 30 {
			check = check[:30]
		}
		check = strings.TrimSpace(check)
		// If we can find even a small prefix of the sent text, it's stuck
		if len(check) >= 4 && strings.Contains(inputAreaContent, check) {
			return true
		}
		// Or if there's substantial content in the input area
		if len(inputAreaContent) > 10 {
			return true
		}
	}

	return false
}

func killTmuxSession(name string) error {
	cmd := exec.Command(tmuxPath, "kill-session", "-t", name)
	return cmd.Run()
}

func listTmuxSessions() ([]string, error) {
	cmd := exec.Command(tmuxPath, "list-sessions", "-F", "#{session_name}")
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	var sessions []string
	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		name := scanner.Text()
		if strings.HasPrefix(name, "claude-") {
			sessions = append(sessions, strings.TrimPrefix(name, "claude-"))
		}
	}
	return sessions, nil
}
