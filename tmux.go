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
)

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

	// Ensure CLAUDECODE is not in tmux global env (prevents "nested session" errors)
	exec.Command(tmuxPath, "set-environment", "-g", "-u", "CLAUDECODE").Run()

	// Create tmux session with a login shell (don't run command directly - it kills session on exit)
	args := []string{"new-session", "-d", "-s", name, "-c", workDir}
	cmd := exec.Command(tmuxPath, args...)
	if err := cmd.Run(); err != nil {
		return err
	}

	// Enable mouse mode for this session (allows scrolling)
	exec.Command(tmuxPath, "set-option", "-t", name, "mouse", "on").Run()

	// Unset CLAUDECODE in session env too, in case it was inherited
	exec.Command(tmuxPath, "set-environment", "-t", name, "-u", "CLAUDECODE").Run()

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

// isClaudeReady checks if Claude Code is at the input prompt (❯) at the bottom of the pane.
// This is more precise than just checking for ❯ anywhere (which could be in scrollback).
func isClaudeReady(session string) bool {
	cmd := exec.Command(tmuxPath, "capture-pane", "-t", session, "-p")
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	// Check the last few non-empty lines for the prompt
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	// Check last 15 lines — Claude HUD status line can push prompt 6+ lines up
	start := len(lines) - 15
	if start < 0 {
		start = 0
	}
	for _, line := range lines[start:] {
		if strings.Contains(line, "❯") {
			return true
		}
	}
	return false
}

// waitForClaude polls the tmux pane until Claude Code's input prompt appears
func waitForClaude(session string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if isClaudeReady(session) {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("timeout waiting for Claude to be ready")
}

// dismissFeedbackDialog checks if Claude Code's "How is Claude doing?" feedback
// dialog is visible in the pane and dismisses it by sending "0" (Dismiss).
// This prevents the dialog from eating Enter keys intended for message submission.
func dismissFeedbackDialog(session string) {
	cmd := exec.Command(tmuxPath, "capture-pane", "-t", session, "-p")
	out, err := cmd.Output()
	if err != nil {
		return
	}
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	// Check last 15 lines for the feedback dialog
	start := len(lines) - 15
	if start < 0 {
		start = 0
	}
	for _, line := range lines[start:] {
		if strings.Contains(line, "How is Claude doing") {
			// Send "0" to dismiss the dialog
			exec.Command(tmuxPath, "send-keys", "-t", session, "-l", "0").Run()
			time.Sleep(300 * time.Millisecond)
			return
		}
	}
}

// sessionQueue manages a per-session message queue so messages are sent
// one at a time, each waiting for Claude to be ready before sending.
type sessionQueue struct {
	ch chan string
}

var (
	sessionQueues   = make(map[string]*sessionQueue)
	sessionQueuesMu sync.Mutex
)

func getSessionQueue(session string) *sessionQueue {
	sessionQueuesMu.Lock()
	defer sessionQueuesMu.Unlock()
	if q, ok := sessionQueues[session]; ok {
		return q
	}
	q := &sessionQueue{ch: make(chan string, 100)}
	sessionQueues[session] = q
	go q.run(session)
	return q
}

func (q *sessionQueue) run(session string) {
	for text := range q.ch {
		fmt.Fprintf(os.Stderr, "queue[%s]: waiting for Claude to be ready, msg: %q\n", session, truncateMsg(text, 80))
		// Wait for Claude to be ready (prompt visible)
		for i := 0; i < 1200; i++ { // 10 minutes max (1200 * 500ms)
			if isClaudeReady(session) {
				break
			}
			time.Sleep(500 * time.Millisecond)
		}
		// Extra settle time after prompt appears
		time.Sleep(500 * time.Millisecond)
		dismissFeedbackDialog(session)

		fmt.Fprintf(os.Stderr, "queue[%s]: Claude ready, sending queued message\n", session)
		if err := sendToTmuxReliable(session, text); err != nil {
			fmt.Fprintf(os.Stderr, "queue[%s]: failed to send: %v\n", session, err)
		}
		// Wait for Claude to start processing before checking next message
		time.Sleep(3 * time.Second)
	}
}

func sendToTmux(session string, text string) error {
	// Dismiss any active feedback dialog that could eat our Enter keys
	dismissFeedbackDialog(session)

	if isClaudeReady(session) {
		// Claude is ready — send immediately
		fmt.Fprintf(os.Stderr, "sendToTmux: Claude ready on %s, sending immediately: %q\n", session, truncateMsg(text, 80))
		return sendToTmuxReliable(session, text)
	}

	// Claude is busy — queue the message for delivery when ready
	fmt.Fprintf(os.Stderr, "sendToTmux: Claude busy on %s, queuing message: %q\n", session, truncateMsg(text, 80))
	q := getSessionQueue(session)
	q.ch <- text
	return nil
}

// truncateMsg is like truncate in hooks.go but for tmux logging
func truncateMsg(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// sendToTmuxReliable sends a message and verifies it was accepted by Claude.
// It retries the Enter key if Claude doesn't start processing within a few seconds.
func sendToTmuxReliable(session string, text string) error {
	// Clear any stale input first by sending Ctrl+U (clear line)
	exec.Command(tmuxPath, "send-keys", "-t", session, "C-u").Run()
	time.Sleep(100 * time.Millisecond)

	// Send text literally
	cmd := exec.Command(tmuxPath, "send-keys", "-t", session, "-l", text)
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "sendToTmux: send-keys -l failed on %s: %v\n", session, err)
		return err
	}

	// Wait for text to appear in the buffer
	baseDelay := 100 * time.Millisecond
	charDelay := time.Duration(len(text)) * time.Millisecond
	delay := baseDelay + charDelay
	if delay > 5*time.Second {
		delay = 5 * time.Second
	}
	time.Sleep(delay)

	// Dismiss feedback dialog again (could have appeared while typing)
	dismissFeedbackDialog(session)

	// Send Enter and verify Claude started processing
	for attempt := 0; attempt < 3; attempt++ {
		cmd = exec.Command(tmuxPath, "send-keys", "-t", session, "Enter")
		if err := cmd.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "sendToTmux: Enter failed on %s attempt %d: %v\n", session, attempt, err)
			return err
		}
		time.Sleep(100 * time.Millisecond)

		// Send a second Enter (Claude Code sometimes needs it for multiline input)
		exec.Command(tmuxPath, "send-keys", "-t", session, "Enter").Run()

		// Wait a moment then check if Claude started processing (prompt should disappear)
		time.Sleep(1500 * time.Millisecond)

		if !isClaudeReady(session) {
			// Claude is processing — message was accepted
			fmt.Fprintf(os.Stderr, "sendToTmux: message accepted on %s (attempt %d)\n", session, attempt)
			return nil
		}

		// Claude is still at prompt — Enter might not have submitted. Retry.
		fmt.Fprintf(os.Stderr, "sendToTmux: still at prompt on %s after attempt %d, retrying Enter\n", session, attempt)
		dismissFeedbackDialog(session)
		time.Sleep(500 * time.Millisecond)
	}

	// After 3 attempts, log but don't error — the message text IS in the buffer
	fmt.Fprintf(os.Stderr, "sendToTmux: WARNING — message may not have submitted on %s after 3 attempts\n", session)
	return nil
}

func sendToTmuxWithDelay(session string, text string, delay time.Duration) error {
	// Send text literally
	cmd := exec.Command(tmuxPath, "send-keys", "-t", session, "-l", text)
	if err := cmd.Run(); err != nil {
		return err
	}

	// Wait for content to load (e.g., images)
	time.Sleep(delay)

	// Send Enter twice (Claude Code needs double Enter)
	cmd = exec.Command(tmuxPath, "send-keys", "-t", session, "Enter")
	if err := cmd.Run(); err != nil {
		return err
	}
	time.Sleep(100 * time.Millisecond)
	cmd = exec.Command(tmuxPath, "send-keys", "-t", session, "Enter")
	return cmd.Run()
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
