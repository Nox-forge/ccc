package main

import (
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// SkillReadiness describes whether a skill can run on this system.
type SkillReadiness struct {
	Ready           bool
	OSCompatible    bool
	MissingBins     []string
	MissingEnv      []string
	MissingConfig   []string
	AvailableAnyBin string // which of the anyBins was found (empty if none)
	HasBaseDir      bool
}

// checkSkillReadiness evaluates whether a skill can function on this system.
func checkSkillReadiness(skill *SkillInfo, config *Config) *SkillReadiness {
	r := &SkillReadiness{
		OSCompatible: true,
	}

	if skill.Meta == nil || skill.Meta.Metadata == nil {
		// No metadata — assume ready
		r.Ready = true
		return r
	}

	meta := skill.Meta.Metadata

	// Check OS compatibility
	if len(meta.OS) > 0 {
		r.OSCompatible = false
		currentOS := runtime.GOOS // "linux", "darwin", "windows"
		for _, supported := range meta.OS {
			// Map OpenClaw OS names to Go runtime names
			mapped := mapOSName(supported)
			if mapped == currentOS {
				r.OSCompatible = true
				break
			}
		}
	}

	// Check required binaries
	if meta.Requires != nil {
		for _, bin := range meta.Requires.Bins {
			if _, err := exec.LookPath(bin); err != nil {
				r.MissingBins = append(r.MissingBins, bin)
			}
		}

		// Check anyBins — at least one must be available
		if len(meta.Requires.AnyBins) > 0 {
			found := false
			for _, bin := range meta.Requires.AnyBins {
				if _, err := exec.LookPath(bin); err == nil {
					r.AvailableAnyBin = bin
					found = true
					break
				}
			}
			if !found {
				// All anyBins are missing — report them
				r.MissingBins = append(r.MissingBins, meta.Requires.AnyBins...)
			}
		}

		// Check required env vars
		for _, env := range meta.Requires.Env {
			if os.Getenv(env) == "" {
				r.MissingEnv = append(r.MissingEnv, env)
			}
		}

		// Check config paths
		for _, cfgPath := range meta.Requires.Config {
			if !checkConfigPath(cfgPath, config) {
				r.MissingConfig = append(r.MissingConfig, cfgPath)
			}
		}
	}

	// Check for {baseDir} in the skill body
	skillMD := skill.Path
	if skill.IsDir {
		skillMD = skill.Path + "/SKILL.md"
	}
	if body, err := getSkillBody(skillMD); err == nil {
		r.HasBaseDir = hasBaseDir(body)
	}

	// Determine overall readiness
	r.Ready = r.OSCompatible &&
		len(r.MissingBins) == 0 &&
		len(r.MissingEnv) == 0 &&
		len(r.MissingConfig) == 0

	return r
}

// mapOSName maps OpenClaw OS names to Go runtime.GOOS values.
func mapOSName(os string) string {
	switch os {
	case "win32":
		return "windows"
	case "darwin", "linux", "windows":
		return os
	default:
		return os
	}
}

// checkConfigPath checks if a dotted config path is configured in CCC.
// e.g. "channels.discord" checks if config.Channels.Discord != nil
func checkConfigPath(path string, config *Config) bool {
	if config == nil {
		return false
	}

	parts := strings.SplitN(path, ".", 2)
	if len(parts) == 0 {
		return false
	}

	switch parts[0] {
	case "channels":
		if config.Channels == nil {
			return false
		}
		if len(parts) < 2 {
			return true
		}
		switch parts[1] {
		case "discord":
			return config.Channels.Discord != nil && config.Channels.Discord.Enabled
		case "signal":
			return config.Channels.Signal != nil && config.Channels.Signal.Enabled
		}
	case "gateway":
		return config.Gateway != nil && config.Gateway.Enabled
	case "plugins":
		// Generic plugin config check — for now always false
		// (CCC doesn't have a plugins system yet)
		return false
	}

	return false
}
