package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// SignalChannel implements Channel for Signal messenger via signal-cli.
type SignalChannel struct {
	mu      sync.Mutex
	config  *Config
	cliPath string
	handler func(msg InboundMessage)
	cancel  chan struct{}
}

// signalEnvelope represents a JSON envelope from signal-cli receive --json.
type signalEnvelope struct {
	Envelope struct {
		Source      string `json:"source"`
		SourceName string `json:"sourceName"`
		DataMessage *struct {
			Message string `json:"message"`
		} `json:"dataMessage"`
	} `json:"envelope"`
}

func NewSignalChannel(config *Config) *SignalChannel {
	cliPath := findSignalCLI()
	return &SignalChannel{
		config:  config,
		cliPath: cliPath,
		cancel:  make(chan struct{}),
	}
}

func (s *SignalChannel) Name() string { return "signal" }

func (s *SignalChannel) Send(session string, msg OutboundMessage) error {
	// Find Signal number for this session
	sigNum := getSignalNumber(s.config, session)
	if sigNum == "" {
		return nil // No Signal config for this session, skip silently
	}

	// Format message
	content := msg.Content
	if msg.Event == "stop" {
		content = fmt.Sprintf("[%s] Done: %s", session, content)
	} else if msg.Event == "notification" {
		content = fmt.Sprintf("[%s] %s", session, content)
	}

	sendSignalMessage(sigNum, content)
	return nil
}

func (s *SignalChannel) Start(handler func(msg InboundMessage)) error {
	s.handler = handler

	if s.cliPath == "" {
		fmt.Fprintf(os.Stderr, "signal: signal-cli not found, channel disabled\n")
		return nil
	}

	// Build session->signal number reverse map
	numberToSession := make(map[string]string)
	for name, info := range s.config.Sessions {
		if info != nil && info.SignalNumber != "" {
			numberToSession[info.SignalNumber] = name
		}
	}

	if len(numberToSession) == 0 {
		fmt.Fprintf(os.Stderr, "signal: no sessions with signal numbers configured\n")
		return nil
	}

	// Start signal-cli receive loop
	go func() {
		for {
			select {
			case <-s.cancel:
				return
			default:
			}

			cmd := exec.Command(s.cliPath, "receive", "--json", "--timeout", "30")
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				fmt.Fprintf(os.Stderr, "signal: pipe error: %v\n", err)
				continue
			}

			if err := cmd.Start(); err != nil {
				fmt.Fprintf(os.Stderr, "signal: start error: %v\n", err)
				continue
			}

			scanner := bufio.NewScanner(stdout)
			for scanner.Scan() {
				var env signalEnvelope
				if err := json.Unmarshal(scanner.Bytes(), &env); err != nil {
					continue
				}

				if env.Envelope.DataMessage == nil || env.Envelope.DataMessage.Message == "" {
					continue
				}

				source := env.Envelope.Source
				message := env.Envelope.DataMessage.Message

				// Map source number to session
				session, ok := numberToSession[source]
				if !ok {
					fmt.Fprintf(os.Stderr, "signal: unknown sender %s\n", source)
					continue
				}

				handler(InboundMessage{
					Session: session,
					Content: message,
					Channel: "signal",
					UserID:  source,
				})
			}

			cmd.Wait()
		}
	}()

	return nil
}

func (s *SignalChannel) Stop() error {
	close(s.cancel)
	return nil
}

// findSignalCLI searches for signal-cli binary.
func findSignalCLI() string {
	// Check OpenClaw's installed signal-cli first
	home, _ := os.UserHomeDir()
	ocSignalDir := filepath.Join(home, ".openclaw", "tools", "signal-cli")
	if info, err := os.Stat(ocSignalDir); err == nil && info.IsDir() {
		entries, _ := os.ReadDir(ocSignalDir)
		for _, e := range entries {
			cliPath := filepath.Join(ocSignalDir, e.Name(), "signal-cli")
			if _, err := os.Stat(cliPath); err == nil {
				return cliPath
			}
		}
	}

	// Check PATH
	if path, err := exec.LookPath("signal-cli"); err == nil {
		return path
	}

	// Check common locations
	candidates := []string{
		"/usr/local/bin/signal-cli",
		filepath.Join(home, ".local", "bin", "signal-cli"),
		filepath.Join(home, "bin", "signal-cli"),
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}

	return ""
}

// findSendSignal searches for send-signal script (existing CCC helper).
func findSendSignal() string {
	home, _ := os.UserHomeDir()
	candidates := []string{
		filepath.Join(home, "bin", "send-signal"),
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	if path, err := exec.LookPath("send-signal"); err == nil {
		return path
	}
	return ""
}

// isSignalConfigured returns true if any session has a signal number configured
// and signal-cli is available.
func isSignalConfigured(config *Config) bool {
	for _, info := range config.Sessions {
		if info != nil && info.SignalNumber != "" {
			return true
		}
	}
	return false
}

// isSignalAvailable returns true if signal-cli is available.
func isSignalAvailable() bool {
	return findSignalCLI() != "" || findSendSignal() != ""
}

// mapSignalToSession returns the session name for a Signal phone number.
func mapSignalToSession(config *Config, number string) string {
	number = strings.TrimSpace(number)
	for name, info := range config.Sessions {
		if info != nil && info.SignalNumber == number {
			return name
		}
	}
	return ""
}
