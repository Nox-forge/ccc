package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

func getConfigPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".ccc.json")
}

func loadConfig() (*Config, error) {
	data, err := os.ReadFile(getConfigPath())
	if err != nil {
		return nil, err
	}

	// First check if this is old format (sessions as map[string]int64)
	var rawConfig map[string]json.RawMessage
	if err := json.Unmarshal(data, &rawConfig); err != nil {
		return nil, err
	}

	// Try to detect old sessions format
	var needsMigration bool
	var oldSessions map[string]int64
	if sessionsRaw, ok := rawConfig["sessions"]; ok {
		// Try to parse as old format (map of topic IDs)
		if json.Unmarshal(sessionsRaw, &oldSessions) == nil && len(oldSessions) > 0 {
			// Check if values are positive numbers (old format)
			for _, v := range oldSessions {
				if v > 0 {
					needsMigration = true
					break
				}
			}
		}
	}

	var config Config
	if needsMigration {
		// Parse everything except sessions first
		type ConfigWithoutSessions struct {
			BotToken    string `json:"bot_token"`
			ChatID      int64  `json:"chat_id"`
			GroupID     int64  `json:"group_id"`
			ProjectsDir string `json:"projects_dir"`
			Away        bool   `json:"away"`
		}
		var partial ConfigWithoutSessions
		json.Unmarshal(data, &partial)

		config.BotToken = partial.BotToken
		config.ChatID = partial.ChatID
		config.GroupID = partial.GroupID
		config.ProjectsDir = partial.ProjectsDir
		config.Away = partial.Away

		// Migrate sessions
		home, _ := os.UserHomeDir()
		config.Sessions = make(map[string]*SessionInfo)
		for name, topicID := range oldSessions {
			// For old sessions, try to figure out the path
			var sessionPath string
			if strings.HasPrefix(name, "/") {
				// Absolute path
				sessionPath = name
			} else if strings.HasPrefix(name, "~/") {
				// Home-relative path
				sessionPath = filepath.Join(home, name[2:])
			} else if config.ProjectsDir != "" {
				// Use projects_dir if set
				projectsDir := config.ProjectsDir
				if strings.HasPrefix(projectsDir, "~/") {
					projectsDir = filepath.Join(home, projectsDir[2:])
				}
				sessionPath = filepath.Join(projectsDir, name)
			} else {
				sessionPath = filepath.Join(home, name)
			}
			config.Sessions[name] = &SessionInfo{
				TopicID: topicID,
				Path:    sessionPath,
			}
		}
		// Save migrated config
		saveConfig(&config)
	} else {
		// Parse with new format
		if err := json.Unmarshal(data, &config); err != nil {
			return nil, err
		}
	}

	if config.Sessions == nil {
		config.Sessions = make(map[string]*SessionInfo)
	}

	return &config, nil
}

func saveConfig(config *Config) error {
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(getConfigPath(), data, 0600)
}

// getProjectsDir returns the base directory for projects
func getProjectsDir(config *Config) string {
	if config.ProjectsDir != "" {
		// Expand ~ to home directory
		if strings.HasPrefix(config.ProjectsDir, "~/") {
			home, _ := os.UserHomeDir()
			return filepath.Join(home, config.ProjectsDir[2:])
		}
		return config.ProjectsDir
	}
	home, _ := os.UserHomeDir()
	return home
}

// resolveProjectPath resolves the full path for a project
// If name starts with / or ~/, it's treated as absolute/home-relative path
// Otherwise, it's relative to projects_dir
func resolveProjectPath(config *Config, name string) string {
	// Absolute path
	if strings.HasPrefix(name, "/") {
		return name
	}
	// Home-relative path (~/something or just ~)
	if strings.HasPrefix(name, "~/") || name == "~" {
		home, _ := os.UserHomeDir()
		if name == "~" {
			return home
		}
		return filepath.Join(home, name[2:])
	}
	// Relative to projects_dir
	return filepath.Join(getProjectsDir(config), name)
}

// expandPath expands ~ to home directory
func expandPath(path string) string {
	if strings.HasPrefix(path, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, path[2:])
	}
	return path
}

// migrateFromOpenClaw imports settings from OpenClaw config on first run.
// Only copies settings that aren't already configured in CCC.
func migrateFromOpenClaw(config *Config) bool {
	home, _ := os.UserHomeDir()
	ocPath := filepath.Join(home, ".openclaw", "openclaw.json")

	data, err := os.ReadFile(ocPath)
	if err != nil {
		return false
	}

	var ocConfig map[string]interface{}
	if err := json.Unmarshal(data, &ocConfig); err != nil {
		return false
	}

	changed := false

	// Import gateway settings
	if config.Gateway == nil {
		if gw, ok := ocConfig["gateway"].(map[string]interface{}); ok {
			config.Gateway = &GatewayConfig{Enabled: true}
			if port, ok := gw["port"].(float64); ok {
				config.Gateway.Port = int(port)
			}
			if auth, ok := gw["auth"].(map[string]interface{}); ok {
				if token, ok := auth["token"].(string); ok {
					config.Gateway.Token = token
				}
			}
			changed = true
		}
	}

	// Import Signal settings
	if config.Channels == nil || config.Channels.Signal == nil {
		if channels, ok := ocConfig["channels"].(map[string]interface{}); ok {
			if sig, ok := channels["signal"].(map[string]interface{}); ok {
				if config.Channels == nil {
					config.Channels = &ChannelsConfig{}
				}
				config.Channels.Signal = &SignalConfig{
					Enabled: true,
				}
				if cliPath, ok := sig["cliPath"].(string); ok {
					config.Channels.Signal.CLIPath = cliPath
				}
				changed = true
			}
		}
	}

	return changed
}
