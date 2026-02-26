package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/mutablelogic/go-whisper/pkg/schema"
	whisper "github.com/mutablelogic/go-whisper/pkg/whisper"
)

func sessionName(name string) string {
	// Replace dots with underscores - tmux interprets dots as window/pane separators
	safeName := strings.ReplaceAll(name, ".", "_")
	return "claude-" + safeName
}

func createSession(config *Config, name string) error {
	// Check if session already exists
	if _, exists := config.Sessions[name]; exists {
		return fmt.Errorf("session '%s' already exists", name)
	}

	// Create Telegram topic
	topicID, err := createForumTopic(config, name)
	if err != nil {
		return fmt.Errorf("failed to create topic: %w", err)
	}

	// Create tmux session
	workDir := resolveProjectPath(config, name)
	if _, err := os.Stat(workDir); os.IsNotExist(err) {
		// Create project directory
		os.MkdirAll(workDir, 0755)
	}

	if err := createTmuxSession(sessionName(name), workDir, false); err != nil {
		return fmt.Errorf("failed to create tmux session: %w", err)
	}

	// Save mapping with full path
	config.Sessions[name] = &SessionInfo{
		TopicID: topicID,
		Path:    workDir,
	}
	if err := saveConfig(config); err != nil {
		return fmt.Errorf("failed to save config: %w", err)
	}

	// Sync session to persistence store
	syncSessionToStore(name, topicID, workDir)

	return nil
}

// syncSessionToStore persists session info to SQLite (fire-and-forget).
func syncSessionToStore(name string, topicID int64, path string) {
	go func() {
		defer func() { recover() }()
		if err := initStore(); err != nil {
			return
		}
		store.UpsertSession(name, topicID, path)
	}()
}

func killSession(config *Config, name string) error {
	if _, exists := config.Sessions[name]; !exists {
		return fmt.Errorf("session '%s' not found", name)
	}

	// Kill tmux session
	killTmuxSession(sessionName(name))

	// Remove from config
	delete(config.Sessions, name)
	saveConfig(config)

	return nil
}

func getSessionByTopic(config *Config, topicID int64) string {
	for name, info := range config.Sessions {
		if info != nil && info.TopicID == topicID {
			return name
		}
	}
	return ""
}

// startSession creates/attaches to a tmux session with Telegram topic
func startSession(continueSession bool) error {
	// Get current directory name as session name
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	name := filepath.Base(cwd)
	tmuxName := sessionName(name)

	// Load config to check/create topic
	config, err := loadConfig()
	if err != nil {
		// No config, just run claude directly
		return runClaudeRaw(continueSession)
	}

	// Create topic if it doesn't exist and we have a group configured
	if config.GroupID != 0 {
		if _, exists := config.Sessions[name]; !exists {
			topicID, err := createForumTopic(config, name)
			if err == nil {
				config.Sessions[name] = &SessionInfo{
					TopicID: topicID,
					Path:    cwd,
				}
				saveConfig(config)
				fmt.Printf("📱 Created Telegram topic: %s\n", name)
			}
		}
	}

	// Check if tmux session exists
	if tmuxSessionExists(tmuxName) {
		// Check if we're already inside tmux
		if os.Getenv("TMUX") != "" {
			// Inside tmux: switch to the session
			cmd := exec.Command(tmuxPath, "switch-client", "-t", tmuxName)
			cmd.Stdin = os.Stdin
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			return cmd.Run()
		}
		// Outside tmux: attach to existing session
		cmd := exec.Command(tmuxPath, "attach-session", "-t", tmuxName)
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd.Run()
	}

	// Create new tmux session and attach
	if err := createTmuxSession(tmuxName, cwd, continueSession); err != nil {
		return err
	}

	// Check if we're already inside tmux
	if os.Getenv("TMUX") != "" {
		cmd := exec.Command(tmuxPath, "switch-client", "-t", tmuxName)
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd.Run()
	}
	cmd := exec.Command(tmuxPath, "attach-session", "-t", tmuxName)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// getContextPrimer builds a context restoration prompt from the latest snapshot
// and recent message history. Returns empty string if no context is available.
func getContextPrimer(sessionName string) string {
	if err := initStore(); err != nil {
		return ""
	}

	var parts []string

	// Get latest snapshot
	snap, err := store.GetLatestSnapshot(sessionName)
	if err == nil && snap != nil {
		parts = append(parts, fmt.Sprintf("## Previous Context Summary\n%s", snap.Summary))
	}

	// Get recent messages since the snapshot (or last 20 if no snapshot)
	var messages []Message
	if snap != nil {
		messages, _ = store.GetMessagesSince(sessionName, snap.Timestamp)
	} else {
		messages, _ = store.GetMessages(sessionName, 20)
	}

	if len(messages) > 0 {
		var msgLines []string
		for _, m := range messages {
			prefix := "User"
			if m.Role == "assistant" {
				prefix = "Assistant"
			}
			// Truncate long messages for the primer
			content := m.Content
			if len(content) > 500 {
				content = content[:500] + "..."
			}
			msgLines = append(msgLines, fmt.Sprintf("**%s** (%s): %s", prefix, m.Channel, content))
		}
		parts = append(parts, fmt.Sprintf("## Recent Conversation\n%s", strings.Join(msgLines, "\n\n")))
	}

	if len(parts) == 0 {
		return ""
	}

	return fmt.Sprintf("<system-reminder>\nThis session was restored. Here is context from the previous session:\n\n%s\n</system-reminder>", strings.Join(parts, "\n\n"))
}

// startDetached creates a Telegram topic, tmux session with Claude, and sends a prompt (no attach)
func startDetached(name string, workDir string, prompt string) error {
	config, err := loadConfig()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	if config.Sessions == nil {
		config.Sessions = make(map[string]*SessionInfo)
	}

	// Create Telegram topic
	topicID, err := createForumTopic(config, name)
	if err != nil {
		return fmt.Errorf("failed to create topic: %w", err)
	}

	tmuxName := sessionName(name)

	// Kill existing tmux session if any
	if tmuxSessionExists(tmuxName) {
		killTmuxSession(tmuxName)
	}

	// Create tmux session (detached)
	if err := createTmuxSession(tmuxName, workDir, false); err != nil {
		return fmt.Errorf("failed to create tmux session: %w", err)
	}

	// Save session info
	config.Sessions[name] = &SessionInfo{
		TopicID: topicID,
		Path:    workDir,
	}
	if err := saveConfig(config); err != nil {
		return fmt.Errorf("failed to save config: %w", err)
	}

	// Wait for Claude to be ready before sending prompt
	if err := waitForClaude(tmuxName, 30*time.Second); err != nil {
		return fmt.Errorf("claude did not start in time: %w", err)
	}

	// Send the prompt to the tmux session
	if err := sendToTmux(tmuxName, prompt); err != nil {
		return fmt.Errorf("failed to send prompt: %w", err)
	}

	fmt.Printf("Session '%s' started in tmux '%s' with topic %d\n", name, tmuxName, topicID)
	return nil
}

const whisperModelName = "ggml-small.bin"
const whisperModelURL = "https://huggingface.co/ggerganov/whisper.cpp/resolve/main/ggml-small.bin"

func getModelsDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".ccc", "models")
}

// ensureModel downloads the whisper model if not present
func ensureModel() (string, error) {
	modelsDir := getModelsDir()
	modelPath := filepath.Join(modelsDir, whisperModelName)
	if _, err := os.Stat(modelPath); err == nil {
		return modelPath, nil
	}

	if err := os.MkdirAll(modelsDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create models dir: %w", err)
	}

	fmt.Printf("Downloading whisper model %s...\n", whisperModelName)
	resp, err := http.Get(whisperModelURL)
	if err != nil {
		return "", fmt.Errorf("failed to download model: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return "", fmt.Errorf("failed to download model: HTTP %d", resp.StatusCode)
	}

	tmpPath := modelPath + ".tmp"
	f, err := os.Create(tmpPath)
	if err != nil {
		return "", fmt.Errorf("failed to create model file: %w", err)
	}

	written, err := io.Copy(f, resp.Body)
	f.Close()
	if err != nil {
		os.Remove(tmpPath)
		return "", fmt.Errorf("failed to write model: %w", err)
	}

	if err := os.Rename(tmpPath, modelPath); err != nil {
		os.Remove(tmpPath)
		return "", fmt.Errorf("failed to rename model: %w", err)
	}

	fmt.Printf("Model downloaded: %s (%d MB)\n", whisperModelName, written/1024/1024)
	return modelPath, nil
}

// Transcribe audio file using native go-whisper
func transcribeAudio(config *Config, audioPath string) (string, error) {
	modelsDir := getModelsDir()

	// Ensure model exists
	if _, err := ensureModel(); err != nil {
		return "", fmt.Errorf("model setup failed: %w", err)
	}

	manager, err := whisper.New(modelsDir)
	if err != nil {
		return "", fmt.Errorf("failed to create whisper manager: %w", err)
	}
	defer manager.Close()

	model := manager.GetModelById("ggml-small")
	if model == nil {
		return "", fmt.Errorf("model ggml-small not found in %s", modelsDir)
	}

	var result strings.Builder
	err = manager.WithModel(model, func(task *whisper.Task) error {
		if config.TranscriptionLang != "" {
			if err := task.SetLanguage(config.TranscriptionLang); err != nil {
				return fmt.Errorf("failed to set language: %w", err)
			}
		}
		f, err := os.Open(audioPath)
		if err != nil {
			return fmt.Errorf("failed to open audio: %w", err)
		}
		defer f.Close()
		return task.TranscribeReader(context.Background(), f, func(seg *schema.Segment) {
			result.WriteString(seg.Text)
		})
	})
	if err != nil {
		return "", fmt.Errorf("transcription failed: %w", err)
	}

	return strings.TrimSpace(result.String()), nil
}
