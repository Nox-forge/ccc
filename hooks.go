package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	imageWarnThreshold    = 70
	imageCompactThreshold = 90
	imageCompactCooldown  = 5 * time.Minute
	imageWarnCooldown     = 10 * time.Minute
)

// fixHookDataCasing handles Claude Code sending camelCase JSON keys instead of snake_case.
// The verbose-hook (Python) already handles both formats. This ensures Go code does too.
func fixHookDataCasing(hookData *HookData, rawData []byte) {
	var raw map[string]json.RawMessage
	if json.Unmarshal(rawData, &raw) != nil {
		return
	}
	if hookData.TranscriptPath == "" {
		if v, ok := raw["transcriptPath"]; ok {
			json.Unmarshal(v, &hookData.TranscriptPath)
		}
	}
	if hookData.HookEventName == "" {
		if v, ok := raw["hookEventName"]; ok {
			json.Unmarshal(v, &hookData.HookEventName)
		}
	}
	if hookData.ToolName == "" {
		if v, ok := raw["toolName"]; ok {
			json.Unmarshal(v, &hookData.ToolName)
		}
	}
	if hookData.SessionID == "" {
		if v, ok := raw["sessionId"]; ok {
			json.Unmarshal(v, &hookData.SessionID)
		}
	}
}

// sendSignalMessage sends a message via Signal using send-signal script.
// It's fire-and-forget (runs in goroutine, errors logged to stderr).
func sendSignalMessage(signalNumber string, message string) {
	go func() {
		defer func() { recover() }()
		cmd := exec.Command("send-signal", message, signalNumber)
		if err := cmd.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "signal: send failed: %v\n", err)
		}
	}()
}

// getSignalNumber returns the signal_number for a session, or empty string
func getSignalNumber(config *Config, sessionName string) string {
	if info, ok := config.Sessions[sessionName]; ok && info != nil {
		return info.SignalNumber
	}
	return ""
}

func handleHook() error {
	config, err := loadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "hook: no config\n")
		return nil
	}

	// Read hook data from stdin
	rawData, _ := io.ReadAll(os.Stdin)
	if len(rawData) == 0 {
		fmt.Fprintf(os.Stderr, "hook: empty stdin\n")
		return nil
	}

	var hookData HookData
	if err := json.Unmarshal(rawData, &hookData); err != nil {
		fmt.Fprintf(os.Stderr, "hook: decode error: %v\n", err)
		return nil
	}

	// Handle camelCase variants from Claude Code
	fixHookDataCasing(&hookData, rawData)

	fmt.Fprintf(os.Stderr, "hook: cwd=%s transcript=%s\n", hookData.Cwd, hookData.TranscriptPath)

	// Find session by matching cwd with saved path
	var sessionName string
	var topicID int64
	for name, info := range config.Sessions {
		if info == nil {
			continue
		}
		// Match against saved path, subdirectories of saved path, or suffix
		if hookData.Cwd == info.Path || strings.HasPrefix(hookData.Cwd, info.Path+"/") || strings.HasSuffix(hookData.Cwd, "/"+name) {
			sessionName = name
			topicID = info.TopicID
			break
		}
	}
	if sessionName == "" || config.GroupID == 0 {
		fmt.Fprintf(os.Stderr, "hook: no session found for cwd=%s\n", hookData.Cwd)
		return nil
	}

	fmt.Fprintf(os.Stderr, "hook: session=%s topic=%d\n", sessionName, topicID)

	// Read last message from transcript with retry (transcript may not be flushed yet)
	lastMessage := "Session ended"
	hookLog("stop: session=%s transcript=%q", sessionName, hookData.TranscriptPath)
	if hookData.TranscriptPath != "" {
		// Try up to 5 times with 500ms delay to wait for transcript flush
		for attempt := 0; attempt < 5; attempt++ {
			msg := getLastAssistantMessage(hookData.TranscriptPath)
			if strings.TrimSpace(msg) != "" {
				lastMessage = msg
				hookLog("stop: got message on attempt %d: %q", attempt, truncate(msg, 80))
				break
			}
			hookLog("stop: attempt %d returned empty/whitespace: %q", attempt, truncate(msg, 40))
			time.Sleep(500 * time.Millisecond)
		}
	} else {
		hookLog("stop: no transcript path!")
	}

	// Check if this message was already sent by a sweep (dedup)
	cacheFile := filepath.Join(os.TempDir(), "ccc-cache-"+sessionName)
	alreadySent := false
	if lastSent, readErr := os.ReadFile(cacheFile); readErr == nil {
		if strings.TrimSpace(string(lastSent)) == strings.TrimSpace(lastMessage) && lastMessage != "Session ended" {
			alreadySent = true
			hookLog("stop: message already sent by sweep, skipping Telegram send")
		}
	}

	// Clear the cache so future PostToolUse hooks don't think this message was sent
	os.Remove(cacheFile)
	msgIDFile := filepath.Join(os.TempDir(), "ccc-msgid-"+sessionName)
	os.Remove(msgIDFile)

	// Persist final assistant message
	persistMessage(sessionName, "assistant", lastMessage, "claude")

	// Save a context snapshot on session stop
	if hookData.TranscriptPath != "" {
		persistSnapshot(sessionName, hookData.TranscriptPath)
	}

	// Notify gateway
	notifyGateway(sessionName, "assistant", lastMessage, "stop")

	// Cancel any pending sweep to prevent duplicates
	sweepFile := filepath.Join(os.TempDir(), "ccc-sweep-"+sessionName)
	os.WriteFile(sweepFile, []byte("stop-cancelled"), 0600)

	// Send only if the sweep didn't already send this message
	if !alreadySent {
		hookLog("stop: SENDING to topic %d (%d chars): %q", topicID, len(lastMessage), truncate(lastMessage, 80))
		err = sendMessage(config, config.GroupID, topicID, fmt.Sprintf("✅ %s\n\n%s", sessionName, lastMessage))
		if err != nil {
			hookLog("stop: SEND FAILED session=%s: %v", sessionName, err)
		} else {
			hookLog("stop: SENT OK session=%s", sessionName)
			os.WriteFile(cacheFile, []byte(lastMessage), 0600)
		}
	}

	// Also send via Signal if configured
	if sigNum := getSignalNumber(config, sessionName); sigNum != "" {
		sendSignalMessage(sigNum, fmt.Sprintf("[%s] Done: %s", sessionName, lastMessage))
	}

	return err
}

func handlePermissionHook() error {
	// Recover from any panic - hooks must never crash
	defer func() {
		recover()
	}()

	// Read stdin with timeout
	stdinData := make(chan []byte, 1)
	go func() {
		defer func() { recover() }()
		data, _ := io.ReadAll(os.Stdin)
		stdinData <- data
	}()

	var rawData []byte
	select {
	case rawData = <-stdinData:
	case <-time.After(2 * time.Second):
		return nil // Timeout, exit silently
	}

	if len(rawData) == 0 {
		return nil
	}

	// Parse JSON - ignore errors
	var hookData HookData
	if err := json.Unmarshal(rawData, &hookData); err != nil {
		return nil
	}

	// Handle camelCase variants from Claude Code
	fixHookDataCasing(&hookData, rawData)

	// Load config - ignore errors
	config, err := loadConfig()
	if err != nil || config == nil {
		return nil
	}

	// Find session by matching cwd with saved path
	var sessionName string
	var topicID int64
	for name, info := range config.Sessions {
		if name == "" || info == nil {
			continue
		}
		// Match against saved path, subdirectories of saved path, or suffix
		if hookData.Cwd == info.Path || strings.HasPrefix(hookData.Cwd, info.Path+"/") || strings.HasSuffix(hookData.Cwd, "/"+name) {
			sessionName = name
			topicID = info.TopicID
			break
		}
	}

	if sessionName == "" || config.GroupID == 0 {
		return nil
	}

	// Handle AskUserQuestion (plan approval, etc.) — synchronous to avoid goroutine data loss
	fmt.Fprintf(os.Stderr, "hook-permission: tool=%s questions=%d\n", hookData.ToolName, len(hookData.ToolInput.Questions))
	if hookData.ToolName == "AskUserQuestion" && len(hookData.ToolInput.Questions) > 0 {
		for qIdx, q := range hookData.ToolInput.Questions {
			if q.Question == "" {
				continue
			}
			// Build message
			msg := fmt.Sprintf("❓ %s\n\n%s", q.Header, q.Question)

			// Build inline keyboard buttons
			var buttons [][]InlineKeyboardButton
			for i, opt := range q.Options {
				if opt.Label == "" {
					continue
				}
				// Callback data format: session:questionIndex:optionIndex
				// Telegram limits callback_data to 64 bytes
				totalQuestions := len(hookData.ToolInput.Questions)
				callbackData := fmt.Sprintf("%s:%d:%d:%d", sessionName, qIdx, totalQuestions, i)
				if len(callbackData) > 64 {
					callbackData = callbackData[:64]
				}
				buttons = append(buttons, []InlineKeyboardButton{
					{Text: opt.Label, CallbackData: callbackData},
				})
			}

			if len(buttons) > 0 {
				sendMessageWithKeyboard(config, config.GroupID, topicID, msg, buttons)
			}

			// Also send via Signal if configured
			if sigNum := getSignalNumber(config, sessionName); sigNum != "" {
				sigMsg := fmt.Sprintf("[%s] %s\n%s", sessionName, q.Header, q.Question)
				for _, opt := range q.Options {
					if opt.Label != "" {
						sigMsg += fmt.Sprintf("\n• %s", opt.Label)
					}
				}
				sendSignalMessage(sigNum, sigMsg)
			}
		}
		return nil
	}

	// Generic permission request — synchronous
	if hookData.ToolName != "" {
		msg := fmt.Sprintf("🔐 Permission requested: %s", hookData.ToolName)
		sendMessage(config, config.GroupID, topicID, msg)
		// Also send via Signal if configured
		if sigNum := getSignalNumber(config, sessionName); sigNum != "" {
			sendSignalMessage(sigNum, fmt.Sprintf("[%s] Permission requested: %s", sessionName, hookData.ToolName))
		}
	}

	return nil
}

func getLastAssistantMessage(transcriptPath string) string {
	file, err := os.Open(transcriptPath)
	if err != nil {
		return ""
	}
	defer file.Close()

	var lastMessage string
	scanner := bufio.NewScanner(file)
	// Increase buffer size for large lines
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 10*1024*1024) // 10MB to handle large tool outputs

	for scanner.Scan() {
		var entry map[string]interface{}
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			continue
		}
		// Reset on user messages so we only return text from the current turn
		if entry["type"] == "user" {
			lastMessage = ""
		}
		if entry["type"] == "assistant" {
			if msg, ok := entry["message"].(map[string]interface{}); ok {
				if content, ok := msg["content"].([]interface{}); ok {
					for _, c := range content {
						if block, ok := c.(map[string]interface{}); ok {
							if block["type"] == "text" {
								if text, ok := block["text"].(string); ok {
									lastMessage = text
								}
							}
						}
					}
				}
			}
		}
	}
	return lastMessage
}

func handlePromptHook() error {
	config, err := loadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "hook-prompt: no config\n")
		return nil
	}

	rawData, _ := io.ReadAll(os.Stdin)
	if len(rawData) == 0 {
		return nil
	}

	var hookData HookData
	if err := json.Unmarshal(rawData, &hookData); err != nil {
		fmt.Fprintf(os.Stderr, "hook-prompt: decode error: %v\n", err)
		return nil
	}

	// Handle camelCase variants from Claude Code
	fixHookDataCasing(&hookData, rawData)

	if hookData.Prompt == "" {
		fmt.Fprintf(os.Stderr, "hook-prompt: empty prompt\n")
		return nil
	}

	// Find session by matching cwd with saved path
	var sessionName string
	var topicID int64
	for name, info := range config.Sessions {
		if info == nil {
			continue
		}
		// Match against saved path, subdirectories of saved path, or suffix
		if hookData.Cwd == info.Path || strings.HasPrefix(hookData.Cwd, info.Path+"/") || strings.HasSuffix(hookData.Cwd, "/"+name) {
			sessionName = name
			topicID = info.TopicID
			break
		}
	}

	if topicID == 0 || config.GroupID == 0 {
		fmt.Fprintf(os.Stderr, "hook-prompt: no topic found for cwd=%s\n", hookData.Cwd)
		return nil
	}

	// Persist user prompt
	if sessionName != "" {
		persistMessage(sessionName, "user", hookData.Prompt, "claude")
	}

	// Cache the current last assistant message to prevent re-sending old messages
	if sessionName != "" && hookData.TranscriptPath != "" {
		if msg := getLastAssistantMessage(hookData.TranscriptPath); msg != "" {
			cacheFile := filepath.Join(os.TempDir(), "ccc-cache-"+sessionName)
			os.WriteFile(cacheFile, []byte(msg), 0600)
		}
	}

	// Enrich the prompt with memory context (goes to Claude only, not Telegram)
	if ctx := getEnrichmentContext(hookData.Prompt); ctx != "" {
		hookOutput := map[string]any{
			"hookSpecificOutput": map[string]any{
				"hookEventName":     "UserPromptSubmit",
				"additionalContext": ctx,
			},
		}
		if outBytes, err := json.Marshal(hookOutput); err == nil {
			fmt.Fprintln(os.Stdout, string(outBytes))
		}

		// Save enrich context for tmux-forward full output channel
		if sessionName != "" {
			enrichFile := filepath.Join(os.TempDir(), "ccc-enrich-"+sessionName)
			os.WriteFile(enrichFile, []byte(ctx), 0600)
		}
	}

	// Send typing action
	sendTypingAction(config, config.GroupID, topicID)

	// Send the prompt to Telegram (sendMessage handles splitting long messages)
	fmt.Fprintf(os.Stderr, "hook-prompt: sending to topic %d\n", topicID)
	err = sendMessage(config, config.GroupID, topicID, fmt.Sprintf("💬 %s", hookData.Prompt))

	// Also send via Signal if configured
	if sessionName != "" {
		if sigNum := getSignalNumber(config, sessionName); sigNum != "" {
			sendSignalMessage(sigNum, fmt.Sprintf("[%s] Prompt: %s", sessionName, hookData.Prompt))
		}
	}

	// Launch a sweep to catch text-only responses (no tool calls).
	// The cache was just updated above (lines 388-393) with the current last
	// assistant message, so the sweep will only send NEW text from Claude's
	// response to this prompt. If Claude uses tools, PostToolUse sweeps will
	// supersede this one.
	if sessionName != "" && hookData.TranscriptPath != "" {
		launchDelayedSweep(sessionName, hookData.TranscriptPath)
	}

	return err
}

func hookLog(format string, args ...interface{}) {
	f, err := os.OpenFile("/tmp/ccc-hook-debug.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s ", time.Now().Format("15:04:05.000"))
	fmt.Fprintf(f, format+"\n", args...)
}

func handleOutputHook() error {
	config, err := loadConfig()
	if err != nil {
		hookLog("output: loadConfig error: %v", err)
		return nil
	}

	rawData, _ := io.ReadAll(os.Stdin)
	if len(rawData) == 0 {
		hookLog("output: empty stdin")
		return nil
	}

	// Log raw JSON keys for debugging
	var rawKeys map[string]json.RawMessage
	json.Unmarshal(rawData, &rawKeys)
	var keyNames []string
	for k := range rawKeys {
		keyNames = append(keyNames, k)
	}
	hookLog("output: raw keys=%v", keyNames)

	var hookData HookData
	if err := json.Unmarshal(rawData, &hookData); err != nil {
		hookLog("output: json decode error: %v", err)
		return nil
	}

	hookLog("output: BEFORE fix - event=%q tool=%q transcript=%q cwd=%q",
		hookData.HookEventName, hookData.ToolName, hookData.TranscriptPath, hookData.Cwd)

	// Handle camelCase variants from Claude Code
	fixHookDataCasing(&hookData, rawData)

	hookLog("output: AFTER fix - event=%q tool=%q transcript=%q",
		hookData.HookEventName, hookData.ToolName, hookData.TranscriptPath)

	// Find session by matching cwd with saved path
	var sessionName string
	var topicID int64
	for name, info := range config.Sessions {
		if info == nil {
			continue
		}
		// Match against saved path, subdirectories of saved path, or suffix
		if hookData.Cwd == info.Path || strings.HasPrefix(hookData.Cwd, info.Path+"/") || strings.HasSuffix(hookData.Cwd, "/"+name) {
			sessionName = name
			topicID = info.TopicID
			break
		}
	}

	if topicID == 0 || config.GroupID == 0 || sessionName == "" {
		hookLog("output: no session for cwd=%s", hookData.Cwd)
		return nil
	}

	hookLog("output: session=%s topic=%d", sessionName, topicID)

	// Get last message from transcript
	if hookData.TranscriptPath == "" {
		hookLog("output: no transcript path!")
		return nil
	}

	msg := getLastAssistantMessage(hookData.TranscriptPath)
	hookLog("output: getLastAssistantMessage returned %d chars: %q", len(msg), truncate(msg, 80))

	if strings.TrimSpace(msg) != "" {
		cacheFile := filepath.Join(os.TempDir(), "ccc-cache-"+sessionName)
		msgIDFile := filepath.Join(os.TempDir(), "ccc-msgid-"+sessionName)
		lockFile := filepath.Join(os.TempDir(), "ccc-lock-"+sessionName)

		// Use file lock to prevent race conditions between parallel hooks
		lock, lockErr := os.OpenFile(lockFile, os.O_CREATE|os.O_RDWR, 0600)
		if lockErr == nil {
			syscall.Flock(int(lock.Fd()), syscall.LOCK_EX)
			defer func() {
				syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
				lock.Close()
			}()
		}

		lastSent, _ := os.ReadFile(cacheFile)
		hookLog("output: cache=%q", truncate(string(lastSent), 80))

		// PostToolUse: try to edit existing message
		if hookData.HookEventName == "PostToolUse" {
			if msgIDData, err := os.ReadFile(msgIDFile); err == nil {
				if msgID, err := strconv.ParseInt(string(msgIDData), 10, 64); err == nil && msgID > 0 {
					// Only edit if message changed (normalize for comparison)
					if strings.TrimSpace(string(lastSent)) != strings.TrimSpace(msg) {
						hookLog("output: editing msg %d", msgID)
						if editErr := editMessage(config, config.GroupID, msgID, topicID, msg); editErr == nil {
							os.WriteFile(cacheFile, []byte(msg), 0600)
						}
						// Persist and notify gateway
						persistMessage(sessionName, "assistant", msg, "claude")
						notifyGateway(sessionName, "assistant", msg, "message")
					}
					return nil
				}
			}
		}

		// PreToolUse or no existing message: check for duplicates, then send new
		// Normalize for comparison (trim whitespace)
		if strings.TrimSpace(string(lastSent)) == strings.TrimSpace(msg) {
			hookLog("output: SKIP duplicate")
			return nil // Skip duplicate
		}

		// Persist assistant message and notify gateway
		persistMessage(sessionName, "assistant", msg, "claude")
		notifyGateway(sessionName, "assistant", msg, "message")

		// Add tool name prefix for PreToolUse
		finalMsg := msg
		if hookData.HookEventName == "PreToolUse" && hookData.ToolName != "" {
			finalMsg = fmt.Sprintf("🔧 %s\n\n%s", hookData.ToolName, msg)
		}

		// Send to Telegram - only update cache on success
		hookLog("output: SENDING to topic %d: %q", topicID, truncate(finalMsg, 80))
		if msgID, err := sendMessageGetID(config, config.GroupID, topicID, finalMsg); err == nil && msgID > 0 {
			os.WriteFile(cacheFile, []byte(msg), 0600)
			os.WriteFile(msgIDFile, []byte(strconv.FormatInt(msgID, 10)), 0600)
			hookLog("output: SENT OK msgID=%d", msgID)
		} else {
			hookLog("output: SEND FAILED for %s: %v", sessionName, err)
		}
	}

	// Check image count on PostToolUse (async, don't block hook)
	if hookData.HookEventName == "PostToolUse" && hookData.TranscriptPath != "" {
		go checkImageCount(config, sessionName, topicID, hookData.TranscriptPath)
	}

	// Also send latest output via Signal if configured (only on PostToolUse to avoid spam)
	if hookData.HookEventName == "PostToolUse" {
		if sigNum := getSignalNumber(config, sessionName); sigNum != "" {
			if hookData.TranscriptPath != "" {
				if msg := getLastAssistantMessage(hookData.TranscriptPath); msg != "" {
					sendSignalMessage(sigNum, fmt.Sprintf("[%s] %s", sessionName, msg))
				}
			}
		}
	}

	// Launch a delayed sweep to catch final text responses that appear AFTER
	// the last tool call (e.g., when Claude responds with just prose).
	// The sweep waits a few seconds, then re-checks the transcript for new text.
	if hookData.HookEventName == "PostToolUse" && hookData.TranscriptPath != "" {
		launchDelayedSweep(sessionName, hookData.TranscriptPath)
	}

	return nil
}

// launchDelayedSweep spawns a background `ccc hook-sweep` process that waits,
// then checks the transcript for unsent assistant text. Uses a sweep timestamp
// file so that only the most recent sweep actually sends (earlier sweeps bow out).
func launchDelayedSweep(sessionName, transcriptPath string) {
	sweepFile := filepath.Join(os.TempDir(), "ccc-sweep-"+sessionName)
	sweepID := fmt.Sprintf("%d", time.Now().UnixNano())
	os.WriteFile(sweepFile, []byte(sweepID), 0600)
	hookLog("sweep: scheduled id=%s session=%s", sweepID, sessionName)

	cmd := exec.Command(cccPath, "hook-sweep", sessionName, transcriptPath, sweepID)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		hookLog("sweep: failed to start: %v", err)
		return
	}
	// Detach — don't wait for child
	go cmd.Wait()
}

// handleSweepHook is called as `ccc hook-sweep <session> <transcript> <sweepID>`.
// It polls the transcript for unsent assistant text over ~45 seconds, checking
// every 3 seconds. This catches final text responses that appear after the last tool.
func handleSweepHook(sessionName, transcriptPath, sweepID string) error {
	hookLog("sweep: started id=%s session=%s", sweepID, sessionName)

	config, err := loadConfig()
	if err != nil {
		return nil
	}

	var topicID int64
	for name, info := range config.Sessions {
		if name == sessionName && info != nil {
			topicID = info.TopicID
			break
		}
	}
	if topicID == 0 {
		return nil
	}

	sweepFile := filepath.Join(os.TempDir(), "ccc-sweep-"+sessionName)
	cacheFile := filepath.Join(os.TempDir(), "ccc-cache-"+sessionName)

	// Poll up to 15 times (3s intervals = ~45 seconds total)
	for attempt := 0; attempt < 15; attempt++ {
		time.Sleep(3 * time.Second)

		// Check if we're still the latest sweep
		currentID, _ := os.ReadFile(sweepFile)
		if string(currentID) != sweepID {
			hookLog("sweep: superseded id=%s attempt=%d (current=%s)", sweepID, attempt, string(currentID))
			return nil
		}

		msg := getLastAssistantMessage(transcriptPath)
		if strings.TrimSpace(msg) == "" {
			hookLog("sweep: no text found id=%s attempt=%d", sweepID, attempt)
			continue
		}

		// Check if this text was already sent
		lastSent, _ := os.ReadFile(cacheFile)
		if strings.TrimSpace(string(lastSent)) == strings.TrimSpace(msg) {
			hookLog("sweep: text unchanged id=%s attempt=%d", sweepID, attempt)
			continue
		}

		// Found new text — send it
		hookLog("sweep: NEW TEXT FOUND id=%s attempt=%d (%d chars): %q", sweepID, attempt, len(msg), truncate(msg, 80))
		break
	}

	// Final check after polling loop
	msg := getLastAssistantMessage(transcriptPath)
	if strings.TrimSpace(msg) == "" {
		hookLog("sweep: giving up, no text id=%s", sweepID)
		return nil
	}

	msgIDFile := filepath.Join(os.TempDir(), "ccc-msgid-"+sessionName)
	lockFile := filepath.Join(os.TempDir(), "ccc-lock-"+sessionName)

	lock, lockErr := os.OpenFile(lockFile, os.O_CREATE|os.O_RDWR, 0600)
	if lockErr != nil {
		return nil
	}
	syscall.Flock(int(lock.Fd()), syscall.LOCK_EX)
	defer func() {
		syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		lock.Close()
	}()

	lastSent, _ := os.ReadFile(cacheFile)
	if strings.TrimSpace(string(lastSent)) == strings.TrimSpace(msg) {
		hookLog("sweep: text unchanged, skip id=%s", sweepID)
		return nil
	}

	hookLog("sweep: SENDING new text id=%s (%d chars): %q", sweepID, len(msg), truncate(msg, 80))

	// Persist and notify
	persistMessage(sessionName, "assistant", msg, "claude")
	notifyGateway(sessionName, "assistant", msg, "message")

	// Try to edit existing message first
	if msgIDData, err := os.ReadFile(msgIDFile); err == nil {
		if msgID, parseErr := strconv.ParseInt(string(msgIDData), 10, 64); parseErr == nil && msgID > 0 {
			if editErr := editMessage(config, config.GroupID, msgID, topicID, msg); editErr == nil {
				os.WriteFile(cacheFile, []byte(msg), 0600)
				hookLog("sweep: EDITED msgID=%d id=%s", msgID, sweepID)
				return nil
			}
		}
	}

	// Send as new message
	if newMsgID, sendErr := sendMessageGetID(config, config.GroupID, topicID, msg); sendErr == nil && newMsgID > 0 {
		os.WriteFile(cacheFile, []byte(msg), 0600)
		os.WriteFile(msgIDFile, []byte(strconv.FormatInt(newMsgID, 10)), 0600)
		hookLog("sweep: SENT OK msgID=%d id=%s", newMsgID, sweepID)
	} else {
		hookLog("sweep: SEND FAILED id=%s: %v", sweepID, sendErr)
	}

	// Also send via Signal if configured
	if sigNum := getSignalNumber(config, sessionName); sigNum != "" {
		sendSignalMessage(sigNum, fmt.Sprintf("[%s] %s", sessionName, msg))
	}

	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// countImagesInTranscript counts base64 image blocks in the transcript
// after the last compact/summary event. Also returns whether the transcript
// contains any summary entries (indicating a compact has occurred).
func countImagesInTranscript(transcriptPath string) (int, bool) {
	if transcriptPath == "" {
		return 0, false
	}

	file, err := os.Open(transcriptPath)
	if err != nil {
		return 0, false
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 10*1024*1024)

	imageCount := 0
	hasSummary := false

	for scanner.Scan() {
		line := scanner.Text()

		// Reset count on compact/summary (images before compact are no longer in context)
		if strings.Contains(line, `"type":"summary"`) {
			imageCount = 0
			hasSummary = true
		}

		// Count base64 image blocks (each represents one image in the API request)
		imageCount += strings.Count(line, `"type":"base64"`)
	}

	return imageCount, hasSummary
}

// checkImageCount checks the image count and warns or auto-compacts.
// It also detects compact events and saves context snapshots.
func checkImageCount(config *Config, sName string, topicID int64, transcriptPath string) {
	defer func() { recover() }()

	imageCount, hasSummary := countImagesInTranscript(transcriptPath)

	// If a summary exists, try to persist a snapshot (idempotent via cooldown)
	if hasSummary {
		snapshotCooldownFile := filepath.Join(os.TempDir(), "ccc-snapshot-"+sName)
		info, err := os.Stat(snapshotCooldownFile)
		if err != nil || time.Since(info.ModTime()) > 2*time.Minute {
			os.WriteFile(snapshotCooldownFile, []byte("1"), 0600)
			persistSnapshot(sName, transcriptPath)
		}
	}

	if imageCount == 0 {
		return
	}

	if imageCount >= imageCompactThreshold {
		// Check cooldown to prevent repeated compacts
		cooldownFile := filepath.Join(os.TempDir(), "ccc-compact-"+sName)
		if info, err := os.Stat(cooldownFile); err == nil {
			if time.Since(info.ModTime()) < imageCompactCooldown {
				return
			}
		}
		os.WriteFile(cooldownFile, []byte(fmt.Sprintf("%d", imageCount)), 0600)
		autoCompact(config, sName, topicID, imageCount)
	} else if imageCount >= imageWarnThreshold {
		// Warn with cooldown to avoid spam
		warnFile := filepath.Join(os.TempDir(), "ccc-imgwarn-"+sName)
		if info, err := os.Stat(warnFile); err == nil {
			if time.Since(info.ModTime()) < imageWarnCooldown {
				return
			}
		}
		os.WriteFile(warnFile, []byte(fmt.Sprintf("%d", imageCount)), 0600)
		sendMessage(config, config.GroupID, topicID,
			fmt.Sprintf("⚠️ Image count: %d/100. Will auto-compact at %d.", imageCount, imageCompactThreshold))
	}
}

// autoCompact interrupts the current Claude session and runs /compact
func autoCompact(config *Config, sName string, topicID int64, imageCount int) {
	tmuxName := "claude-" + strings.ReplaceAll(sName, ".", "_")

	if !tmuxSessionExists(tmuxName) {
		return
	}

	sendMessage(config, config.GroupID, topicID,
		fmt.Sprintf("⚠️ Auto-compacting: %d/100 images in context", imageCount))

	// Interrupt current operation
	exec.Command(tmuxPath, "send-keys", "-t", tmuxName, "Escape").Run()
	time.Sleep(300 * time.Millisecond)
	exec.Command(tmuxPath, "send-keys", "-t", tmuxName, "C-c").Run()

	// Wait for Claude to return to prompt
	if err := waitForClaude(tmuxName, 15*time.Second); err != nil {
		sendMessage(config, config.GroupID, topicID, "⚠️ Auto-compact: could not reach prompt, skipping")
		return
	}

	// Send /compact command
	exec.Command(tmuxPath, "send-keys", "-t", tmuxName, "-l", "/compact").Run()
	time.Sleep(1 * time.Second)
	exec.Command(tmuxPath, "send-keys", "-t", tmuxName, "Enter").Run()

	// Wait for compact to complete
	time.Sleep(15 * time.Second)

	sendMessage(config, config.GroupID, topicID, "✅ Auto-compact completed. Image context cleared.")
}

func handleQuestionHook() error {
	config, err := loadConfig()
	if err != nil {
		return nil
	}

	rawData, _ := io.ReadAll(os.Stdin)
	if len(rawData) == 0 {
		return nil
	}

	var hookData HookData
	if err := json.Unmarshal(rawData, &hookData); err != nil {
		return nil
	}

	// Handle camelCase variants from Claude Code
	fixHookDataCasing(&hookData, rawData)

	// Find session by matching cwd with saved path
	var sessionName string
	var topicID int64
	for name, info := range config.Sessions {
		if info == nil {
			continue
		}
		// Match against saved path, subdirectories of saved path, or suffix
		if hookData.Cwd == info.Path || strings.HasPrefix(hookData.Cwd, info.Path+"/") || strings.HasSuffix(hookData.Cwd, "/"+name) {
			sessionName = name
			topicID = info.TopicID
			break
		}
	}

	if sessionName == "" || config.GroupID == 0 || topicID == 0 {
		return nil
	}

	// Send questions to Telegram and Signal
	for qIdx, q := range hookData.ToolInput.Questions {
		if q.Question == "" {
			continue
		}
		msg := fmt.Sprintf("❓ %s\n\n%s", q.Header, q.Question)

		var buttons [][]InlineKeyboardButton
		for i, opt := range q.Options {
			if opt.Label == "" {
				continue
			}
			totalQuestions := len(hookData.ToolInput.Questions)
			callbackData := fmt.Sprintf("%s:%d:%d:%d", sessionName, qIdx, totalQuestions, i)
			if len(callbackData) > 64 {
				callbackData = callbackData[:64]
			}
			buttons = append(buttons, []InlineKeyboardButton{
				{Text: opt.Label, CallbackData: callbackData},
			})
		}

		if len(buttons) > 0 {
			sendMessageWithKeyboard(config, config.GroupID, topicID, msg, buttons)
		} else {
			sendMessage(config, config.GroupID, topicID, msg)
		}

		// Also send via Signal if configured
		if sigNum := getSignalNumber(config, sessionName); sigNum != "" {
			sigMsg := fmt.Sprintf("[%s] %s\n%s", sessionName, q.Header, q.Question)
			for _, opt := range q.Options {
				if opt.Label != "" {
					sigMsg += fmt.Sprintf("\n• %s", opt.Label)
				}
			}
			sendSignalMessage(sigNum, sigMsg)
		}
	}

	return nil
}

// RalphIterationData represents data from a Ralph loop iteration.
// Sent by the Ralph stop hook to persist and relay each iteration.
type RalphIterationData struct {
	Cwd            string `json:"cwd"`
	TranscriptPath string `json:"transcript_path"`
	Iteration      int    `json:"iteration"`
	MaxIterations  int    `json:"max_iterations"`
	Prompt         string `json:"prompt"`
	LastOutput     string `json:"last_output"`
}

func handleRalphIterationHook() error {
	config, err := loadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "hook-ralph: no config\n")
		return nil
	}

	rawData, _ := io.ReadAll(os.Stdin)
	if len(rawData) == 0 {
		fmt.Fprintf(os.Stderr, "hook-ralph: empty stdin\n")
		return nil
	}

	var data RalphIterationData
	if err := json.Unmarshal(rawData, &data); err != nil {
		fmt.Fprintf(os.Stderr, "hook-ralph: decode error: %v\n", err)
		return nil
	}

	// Find session by matching cwd with saved path
	var sessionName string
	var topicID int64
	for name, info := range config.Sessions {
		if info == nil {
			continue
		}
		if data.Cwd == info.Path || strings.HasPrefix(data.Cwd, info.Path+"/") || strings.HasSuffix(data.Cwd, "/"+name) {
			sessionName = name
			topicID = info.TopicID
			break
		}
	}

	if sessionName == "" || config.GroupID == 0 {
		fmt.Fprintf(os.Stderr, "hook-ralph: no session for cwd=%s\n", data.Cwd)
		return nil
	}

	// Persist the iteration prompt as a user message so the memory system sees both sides
	iterLabel := fmt.Sprintf("[Ralph iteration %d", data.Iteration)
	if data.MaxIterations > 0 {
		iterLabel += fmt.Sprintf("/%d", data.MaxIterations)
	}
	iterLabel += "]"

	promptMsg := fmt.Sprintf("%s\n\n%s", iterLabel, data.Prompt)
	persistMessage(sessionName, "user", promptMsg, "ralph")

	// Persist the last assistant output explicitly (in case hook-output missed it)
	if data.LastOutput != "" {
		persistMessage(sessionName, "assistant", data.LastOutput, "ralph")
	}

	// Notify gateway about the iteration
	notifyGateway(sessionName, "user", promptMsg, "ralph-iteration")

	// Send iteration summary to Telegram
	// Truncate last output for Telegram readability
	outputSummary := data.LastOutput
	if len(outputSummary) > 500 {
		outputSummary = outputSummary[:500] + "..."
	}

	var iterMsg string
	if data.MaxIterations > 0 {
		iterMsg = fmt.Sprintf("🔄 Ralph iteration %d/%d\n\n", data.Iteration, data.MaxIterations)
	} else {
		iterMsg = fmt.Sprintf("🔄 Ralph iteration %d\n\n", data.Iteration)
	}
	if outputSummary != "" {
		iterMsg += fmt.Sprintf("📤 Last output:\n%s", outputSummary)
	}

	if topicID != 0 {
		sendMessage(config, config.GroupID, topicID, iterMsg)
	}

	// Also send via Signal if configured
	if sigNum := getSignalNumber(config, sessionName); sigNum != "" {
		sendSignalMessage(sigNum, fmt.Sprintf("[%s] %s", sessionName, iterMsg))
	}

	return nil
}

// persistMessage saves a message to the store synchronously.
// Must be synchronous in hook processes — fire-and-forget goroutines get killed
// when the short-lived CLI process exits, causing silent data loss.
func persistMessage(session, role, content, channel string) {
	if err := initStore(); err != nil {
		fmt.Fprintf(os.Stderr, "persist: store init error: %v\n", err)
		return
	}
	if err := store.SaveMessage(session, role, content, channel); err != nil {
		fmt.Fprintf(os.Stderr, "persist: save error: %v\n", err)
	}
}

// persistSnapshot saves a context snapshot from a compact event synchronously.
func persistSnapshot(session, transcriptPath string) {
	if err := initStore(); err != nil {
		fmt.Fprintf(os.Stderr, "persist: store init error: %v\n", err)
		return
	}
	// Extract the summary from the transcript (the text after the last "type":"summary" entry)
	summary := getCompactSummary(transcriptPath)
	if summary == "" {
		summary = "Context compacted (no summary extracted)"
	}
	if err := store.SaveSnapshot(session, summary); err != nil {
		fmt.Fprintf(os.Stderr, "persist: snapshot error: %v\n", err)
	} else {
		fmt.Fprintf(os.Stderr, "persist: snapshot saved for session %s (%d bytes)\n", session, len(summary))
	}
}

// getCompactSummary extracts the summary text from the last compact/summary entry in the transcript.
func getCompactSummary(transcriptPath string) string {
	if transcriptPath == "" {
		return ""
	}

	file, err := os.Open(transcriptPath)
	if err != nil {
		return ""
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 10*1024*1024)

	var lastSummary string
	for scanner.Scan() {
		var entry map[string]interface{}
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			continue
		}
		if entry["type"] == "summary" {
			if msg, ok := entry["summary"].(string); ok && msg != "" {
				lastSummary = msg
			}
		}
	}
	return lastSummary
}

func handleNotificationHook() error {
	config, err := loadConfig()
	if err != nil {
		return nil
	}

	rawData, _ := io.ReadAll(os.Stdin)
	if len(rawData) == 0 {
		return nil
	}

	var hookData HookData
	if err := json.Unmarshal(rawData, &hookData); err != nil {
		return nil
	}

	// Handle camelCase variants from Claude Code
	fixHookDataCasing(&hookData, rawData)

	if hookData.Notification == "" {
		return nil
	}

	// Find session by matching cwd with saved path
	var sessionName string
	var topicID int64
	for name, info := range config.Sessions {
		if info == nil {
			continue
		}
		// Match against saved path, subdirectories of saved path, or suffix
		if hookData.Cwd == info.Path || strings.HasPrefix(hookData.Cwd, info.Path+"/") || strings.HasSuffix(hookData.Cwd, "/"+name) {
			sessionName = name
			topicID = info.TopicID
			break
		}
	}

	if topicID == 0 || config.GroupID == 0 {
		return nil
	}

	err = sendMessage(config, config.GroupID, topicID, fmt.Sprintf("🔔 %s", hookData.Notification))

	// Also send via Signal if configured
	if sigNum := getSignalNumber(config, sessionName); sigNum != "" {
		sendSignalMessage(sigNum, fmt.Sprintf("[%s] %s", sessionName, hookData.Notification))
	}

	return err
}

// isCccHook checks if a hook entry contains a ccc command
func isCccHook(entry interface{}) bool {
	// Direct command hook: {"command": "...", "type": "command"}
	if m, ok := entry.(map[string]interface{}); ok {
		if cmd, ok := m["command"].(string); ok {
			return strings.Contains(cmd, "ccc hook")
		}
		// Wrapper hook: {"hooks": [...], "matcher": "..."}
		if hooks, ok := m["hooks"].([]interface{}); ok {
			for _, h := range hooks {
				if hm, ok := h.(map[string]interface{}); ok {
					if cmd, ok := hm["command"].(string); ok {
						if strings.Contains(cmd, "ccc hook") {
							return true
						}
					}
				}
			}
		}
	}
	return false
}

// removeCccHooks removes all ccc hooks from a hook array
func removeCccHooks(hookArray []interface{}) []interface{} {
	var result []interface{}
	for _, entry := range hookArray {
		if !isCccHook(entry) {
			result = append(result, entry)
		}
	}
	return result
}

func installHook() error {
	home, _ := os.UserHomeDir()
	settingsPath := filepath.Join(home, ".claude", "settings.json")

	data, err := os.ReadFile(settingsPath)
	if err != nil {
		return fmt.Errorf("failed to read settings.json: %w", err)
	}

	var settings map[string]interface{}
	if err := json.Unmarshal(data, &settings); err != nil {
		return fmt.Errorf("failed to parse settings.json: %w", err)
	}

	hooks, ok := settings["hooks"].(map[string]interface{})
	if !ok {
		hooks = make(map[string]interface{})
	}

	// Define all ccc hooks to install (new format with matcher and hooks array)
	cccHooks := map[string][]interface{}{
		"Stop": {
			map[string]interface{}{
				"hooks": []interface{}{
					map[string]interface{}{
						"command": cccPath + " hook",
						"type":    "command",
					},
				},
				"matcher": "",
			},
		},
		"Notification": {
			map[string]interface{}{
				"hooks": []interface{}{
					map[string]interface{}{
						"command": cccPath + " hook-notification",
						"type":    "command",
					},
				},
				"matcher": "",
			},
		},
		"PermissionRequest": {
			map[string]interface{}{
				"hooks": []interface{}{
					map[string]interface{}{
						"command": cccPath + " hook-permission",
						"type":    "command",
					},
				},
				"matcher": "",
			},
		},
		"PostToolUse": {
			map[string]interface{}{
				"hooks": []interface{}{
					map[string]interface{}{
						"command": cccPath + " hook-output",
						"type":    "command",
					},
				},
				"matcher": "",
			},
		},
		"PreToolUse": {
			map[string]interface{}{
				"hooks": []interface{}{
					map[string]interface{}{
						"command": cccPath + " hook-question",
						"type":    "command",
					},
				},
				"matcher": "AskUserQuestion",
			},
			map[string]interface{}{
				"hooks": []interface{}{
					map[string]interface{}{
						"command": cccPath + " hook-output",
						"type":    "command",
					},
				},
				"matcher": "",
			},
		},
		"UserPromptSubmit": {
			map[string]interface{}{
				"hooks": []interface{}{
					map[string]interface{}{
						"command": cccPath + " hook-prompt",
						"type":    "command",
					},
				},
				"matcher": "",
			},
		},
	}

	// For each hook type, remove existing ccc hooks and add new ones
	for hookType, newHooks := range cccHooks {
		var existingHooks []interface{}
		if existing, ok := hooks[hookType].([]interface{}); ok {
			existingHooks = removeCccHooks(existing)
		}
		// Add ccc hooks to the beginning
		hooks[hookType] = append(newHooks, existingHooks...)
	}

	settings["hooks"] = hooks

	newData, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal settings: %w", err)
	}

	if err := os.WriteFile(settingsPath, newData, 0600); err != nil {
		return fmt.Errorf("failed to write settings.json: %w", err)
	}

	fmt.Println("✅ Claude hooks installed!")
	return nil
}

func uninstallHook() error {
	home, _ := os.UserHomeDir()
	settingsPath := filepath.Join(home, ".claude", "settings.json")

	data, err := os.ReadFile(settingsPath)
	if err != nil {
		return fmt.Errorf("failed to read settings.json: %w", err)
	}

	var settings map[string]interface{}
	if err := json.Unmarshal(data, &settings); err != nil {
		return fmt.Errorf("failed to parse settings.json: %w", err)
	}

	hooks, ok := settings["hooks"].(map[string]interface{})
	if !ok {
		fmt.Println("No hooks found")
		return nil
	}

	// Remove ccc hooks from each hook type
	hookTypes := []string{"Stop", "Notification", "PermissionRequest", "PostToolUse", "PreToolUse", "UserPromptSubmit"}
	for _, hookType := range hookTypes {
		if existing, ok := hooks[hookType].([]interface{}); ok {
			filtered := removeCccHooks(existing)
			if len(filtered) == 0 {
				delete(hooks, hookType)
			} else {
				hooks[hookType] = filtered
			}
		}
	}

	settings["hooks"] = hooks

	newData, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal settings: %w", err)
	}

	if err := os.WriteFile(settingsPath, newData, 0600); err != nil {
		return fmt.Errorf("failed to write settings.json: %w", err)
	}

	fmt.Println("✅ Claude hooks uninstalled!")
	return nil
}

func installSkill() error {
	home, _ := os.UserHomeDir()
	skillDir := filepath.Join(home, ".claude", "skills")
	skillPath := filepath.Join(skillDir, "ccc-send.md")

	if err := os.MkdirAll(skillDir, 0755); err != nil {
		return fmt.Errorf("failed to create skills directory: %w", err)
	}

	skillContent := `# CCC Send - File Transfer Skill

## Description
Send files to the user via Telegram using the ccc send command.

## Usage
When the user asks you to send them a file, or when you have generated/built a file that the user needs (like an APK, binary, or any other file), use this command:

` + "```bash" + `
ccc send <file_path>
` + "```" + `

## How it works
- **Small files (< 50MB)**: Sent directly via Telegram
- **Large files (≥ 50MB)**: Streamed via relay server with a one-time download link

## Examples

### Send a built APK
` + "```bash" + `
ccc send ./build/app.apk
` + "```" + `

### Send a generated file
` + "```bash" + `
ccc send ./output/report.pdf
` + "```" + `

### Send from subdirectory
` + "```bash" + `
ccc send ~/Downloads/large-file.zip
` + "```" + `

## Important Notes
- The command detects the current session from your working directory
- For large files, the command will wait up to 10 minutes for the user to download
- Each download link is one-time use only
- Use this proactively when you've created files the user needs!
`

	if err := os.WriteFile(skillPath, []byte(skillContent), 0644); err != nil {
		return fmt.Errorf("failed to write skill file: %w", err)
	}

	fmt.Println("✅ CCC send skill installed!")
	return nil
}

func uninstallSkill() error {
	home, _ := os.UserHomeDir()
	skillPath := filepath.Join(home, ".claude", "skills", "ccc-send.md")
	os.Remove(skillPath)
	return nil
}
