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
	"time"
)

const (
	imageWarnThreshold    = 70
	imageCompactThreshold = 90
	imageCompactCooldown  = 5 * time.Minute
	imageWarnCooldown     = 10 * time.Minute
)

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
	var hookData HookData
	decoder := json.NewDecoder(os.Stdin)
	if err := decoder.Decode(&hookData); err != nil {
		fmt.Fprintf(os.Stderr, "hook: decode error: %v\n", err)
		return nil
	}

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

	// Read last message from transcript
	lastMessage := "Session ended"
	if hookData.TranscriptPath != "" {
		if msg := getLastAssistantMessage(hookData.TranscriptPath); msg != "" {
			lastMessage = msg
		}
	}

	// Clear the cache so future PostToolUse hooks don't think this message was sent
	cacheFile := filepath.Join(os.TempDir(), "ccc-cache-"+sessionName)
	os.Remove(cacheFile)
	msgIDFile := filepath.Join(os.TempDir(), "ccc-msgid-"+sessionName)
	os.Remove(msgIDFile)

	// Persist final assistant message
	persistMessage(sessionName, "assistant", lastMessage, "claude")

	// Save a context snapshot on session stop
	if hookData.TranscriptPath != "" {
		persistSnapshot(sessionName, hookData.TranscriptPath)
	}

	// Always send the Stop message (final result)
	err = sendMessage(config, config.GroupID, topicID, fmt.Sprintf("✅ %s\n\n%s", sessionName, lastMessage))

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

	// Handle AskUserQuestion (plan approval, etc.) - in goroutine to not block
	fmt.Fprintf(os.Stderr, "hook-permission: tool=%s questions=%d\n", hookData.ToolName, len(hookData.ToolInput.Questions))
	if hookData.ToolName == "AskUserQuestion" && len(hookData.ToolInput.Questions) > 0 {
		go func() {
			defer func() { recover() }()
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
		}()
		return nil
	}

	// Generic permission request - in goroutine to not block
	go func() {
		defer func() { recover() }()
		if hookData.ToolName != "" {
			msg := fmt.Sprintf("🔐 Permission requested: %s", hookData.ToolName)
			sendMessage(config, config.GroupID, topicID, msg)
			// Also send via Signal if configured
			if sigNum := getSignalNumber(config, sessionName); sigNum != "" {
				sendSignalMessage(sigNum, fmt.Sprintf("[%s] Permission requested: %s", sessionName, hookData.ToolName))
			}
		}
	}()

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

	var hookData HookData
	decoder := json.NewDecoder(os.Stdin)
	if err := decoder.Decode(&hookData); err != nil {
		fmt.Fprintf(os.Stderr, "hook-prompt: decode error: %v\n", err)
		return nil
	}

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

	return err
}

func handleOutputHook() error {
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
		return nil
	}

	// Get last message from transcript
	if hookData.TranscriptPath != "" {
		if msg := getLastAssistantMessage(hookData.TranscriptPath); msg != "" {
			cacheFile := filepath.Join(os.TempDir(), "ccc-cache-"+sessionName)
			msgIDFile := filepath.Join(os.TempDir(), "ccc-msgid-"+sessionName)
			lastSent, _ := os.ReadFile(cacheFile)

			// PostToolUse: try to edit existing message
			if hookData.HookEventName == "PostToolUse" {
				if msgIDData, err := os.ReadFile(msgIDFile); err == nil {
					if msgID, err := strconv.ParseInt(string(msgIDData), 10, 64); err == nil && msgID > 0 {
						// Only edit if message changed (normalize for comparison)
						if strings.TrimSpace(string(lastSent)) != strings.TrimSpace(msg) {
							os.WriteFile(cacheFile, []byte(msg), 0600)
							editMessage(config, config.GroupID, msgID, topicID, msg)
							// Persist the updated assistant message
							persistMessage(sessionName, "assistant", msg, "claude")
						}
						return nil
					}
				}
			}

			// PreToolUse or no existing message: check for duplicates, then send new
			// Normalize for comparison (trim whitespace)
			if strings.TrimSpace(string(lastSent)) == strings.TrimSpace(msg) {
				return nil // Skip duplicate
			}
			os.WriteFile(cacheFile, []byte(msg), 0600)

			// Persist assistant message
			persistMessage(sessionName, "assistant", msg, "claude")

			// Add tool name prefix for PreToolUse
			finalMsg := msg
			if hookData.HookEventName == "PreToolUse" && hookData.ToolName != "" {
				finalMsg = fmt.Sprintf("🔧 %s\n\n%s", hookData.ToolName, msg)
			}

			if msgID, err := sendMessageGetID(config, config.GroupID, topicID, finalMsg); err == nil && msgID > 0 {
				os.WriteFile(msgIDFile, []byte(strconv.FormatInt(msgID, 10)), 0600)
			}
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

	return nil
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

// persistMessage saves a message to the store (fire-and-forget, never blocks hooks).
func persistMessage(session, role, content, channel string) {
	go func() {
		defer func() { recover() }()
		if err := initStore(); err != nil {
			fmt.Fprintf(os.Stderr, "persist: store init error: %v\n", err)
			return
		}
		if err := store.SaveMessage(session, role, content, channel); err != nil {
			fmt.Fprintf(os.Stderr, "persist: save error: %v\n", err)
		}
	}()
}

// persistSnapshot saves a context snapshot from a compact event.
func persistSnapshot(session, transcriptPath string) {
	go func() {
		defer func() { recover() }()
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
	}()
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
