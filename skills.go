package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

// SkillInfo describes a discovered skill.
type SkillInfo struct {
	Name      string           // skill name (directory name or filename without .md)
	Path      string           // absolute path to the skill directory or file
	Source    string           // "clawhub", "openclaw-bundled", "claude-code"
	Synced    bool             // true if present in ~/.claude/skills/
	IsDir     bool             // true if skill is a directory (with SKILL.md), false if flat .md
	Meta      *SkillFrontmatter // parsed frontmatter (populated by enrichSkills)
	Readiness *SkillReadiness   // readiness check result (populated by enrichSkills)
}

// processedMarker is written alongside processed SKILL.md files
// to track whether re-processing is needed.
type processedMarker struct {
	SourcePath string `json:"source_path"`
	SourceHash string `json:"source_hash"`
	Timestamp  string `json:"timestamp"`
}

// getSkillDirs returns all directories to scan for skills.
func getSkillDirs() map[string]string {
	home, _ := os.UserHomeDir()
	return map[string]string{
		"clawhub":          filepath.Join(home, ".openclaw", "skills"),
		"openclaw-bundled": findBundledSkillsDir(),
		"claude-code":      filepath.Join(home, ".claude", "skills"),
	}
}

// findBundledSkillsDir finds OpenClaw's bundled skills from npm install.
func findBundledSkillsDir() string {
	home, _ := os.UserHomeDir()

	// Check common locations for OpenClaw bundled skills
	candidates := []string{
		// npm global install
		filepath.Join(home, ".npm-global", "lib", "node_modules", "openclaw", "skills"),
		// Local project
		filepath.Join(home, "projects", "openclaw", "skills"),
		// Various node_modules locations
		filepath.Join(home, ".openclaw", "tools", "skills"),
		filepath.Join(home, "node_modules", "openclaw", "skills"),
		filepath.Join(home, "node_modules", "@openclaw", "core", "skills"),
	}

	// Also search within .openclaw/node_modules
	ocNodeModules := filepath.Join(home, ".openclaw", "node_modules")
	if info, err := os.Stat(ocNodeModules); err == nil && info.IsDir() {
		entries, _ := os.ReadDir(ocNodeModules)
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), "openclaw") || strings.HasPrefix(e.Name(), "@openclaw") {
				skillsDir := filepath.Join(ocNodeModules, e.Name(), "skills")
				candidates = append(candidates, skillsDir)
			}
		}
	}

	// Also try finding via npm root
	if out, err := exec.Command("npm", "root", "-g").Output(); err == nil {
		globalRoot := strings.TrimSpace(string(out))
		candidates = append(candidates, filepath.Join(globalRoot, "openclaw", "skills"))
	}

	for _, dir := range candidates {
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			// Verify it actually contains skills (subdirs with SKILL.md)
			entries, _ := os.ReadDir(dir)
			for _, e := range entries {
				if e.IsDir() {
					skillMD := filepath.Join(dir, e.Name(), "SKILL.md")
					if _, err := os.Stat(skillMD); err == nil {
						return dir
					}
				}
			}
		}
	}
	return ""
}

// scanSkills discovers all available skills across all sources.
// OpenClaw uses subdirectory format: skills/<name>/SKILL.md
// Claude Code supports both: skills/<name>/SKILL.md and skills/<name>.md
func scanSkills() []SkillInfo {
	home, _ := os.UserHomeDir()
	claudeSkillsDir := filepath.Join(home, ".claude", "skills")
	dirs := getSkillDirs()

	var skills []SkillInfo
	seen := make(map[string]bool)

	for source, dir := range dirs {
		if dir == "" {
			continue
		}
		if _, err := os.Stat(dir); err != nil {
			continue
		}

		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}

		for _, entry := range entries {
			name := entry.Name()

			// Case 1: Subdirectory with SKILL.md (OpenClaw format)
			if entry.IsDir() {
				skillMD := filepath.Join(dir, name, "SKILL.md")
				if _, err := os.Stat(skillMD); err != nil {
					continue // No SKILL.md in this dir
				}

				if seen[name] {
					continue
				}
				seen[name] = true

				fullPath := filepath.Join(dir, name)

				// Check if synced to Claude Code skills dir
				synced := isSynced(claudeSkillsDir, name, fullPath, source == "claude-code")

				skills = append(skills, SkillInfo{
					Name:   name,
					Path:   fullPath,
					Source: source,
					Synced: synced,
					IsDir:  true,
				})
				continue
			}

			// Case 2: Flat .md file (Claude Code native format)
			if !strings.HasSuffix(name, ".md") {
				continue
			}

			baseName := strings.TrimSuffix(name, ".md")
			if seen[baseName] {
				continue
			}
			seen[baseName] = true

			fullPath := filepath.Join(dir, name)
			synced := false
			if source == "claude-code" {
				synced = true // Already in the right place
			} else {
				synced = isSynced(claudeSkillsDir, baseName, fullPath, false)
			}

			skills = append(skills, SkillInfo{
				Name:   baseName,
				Path:   fullPath,
				Source: source,
				Synced: synced,
				IsDir:  false,
			})
		}
	}

	sort.Slice(skills, func(i, j int) bool {
		return skills[i].Name < skills[j].Name
	})

	return skills
}

// enrichSkills populates Meta and Readiness for each skill.
func enrichSkills(skills []SkillInfo, config *Config) {
	for i := range skills {
		skillMD := skills[i].Path
		if skills[i].IsDir {
			skillMD = filepath.Join(skills[i].Path, "SKILL.md")
		}
		fm, err := parseFrontmatter(skillMD)
		if err == nil {
			skills[i].Meta = fm
		}
		skills[i].Readiness = checkSkillReadiness(&skills[i], config)
	}
}

// isSynced checks if a skill is present in the Claude Code skills directory.
func isSynced(claudeSkillsDir, name, sourcePath string, isNative bool) bool {
	if isNative {
		return true
	}

	// Check for directory symlink: ~/.claude/skills/<name> -> sourcePath
	dirLink := filepath.Join(claudeSkillsDir, name)
	if target, err := os.Readlink(dirLink); err == nil {
		return target == sourcePath
	}
	// Check if a directory exists (maybe copied or processed)
	if info, err := os.Stat(dirLink); err == nil && info.IsDir() {
		skillMD := filepath.Join(dirLink, "SKILL.md")
		if _, err := os.Stat(skillMD); err == nil {
			return true
		}
	}

	// Check for flat file symlink: ~/.claude/skills/<name>.md -> sourcePath
	fileLink := filepath.Join(claudeSkillsDir, name+".md")
	if target, err := os.Readlink(fileLink); err == nil {
		return target == sourcePath
	}

	return false
}

// syncSkills creates symlinks for all non-claude-code skills into ~/.claude/skills/.
// When forceAll is false, OS-incompatible skills are excluded.
// For skills using {baseDir}, creates a processed copy instead of a symlink.
func syncSkills(forceAll bool) (synced, skipped, excluded int, err error) {
	home, _ := os.UserHomeDir()
	claudeSkillsDir := filepath.Join(home, ".claude", "skills")

	if mkErr := os.MkdirAll(claudeSkillsDir, 0755); mkErr != nil {
		return 0, 0, 0, fmt.Errorf("create skills dir: %w", mkErr)
	}

	config, _ := loadConfig()
	skills := scanSkills()
	enrichSkills(skills, config)

	for _, skill := range skills {
		if skill.Source == "claude-code" {
			skipped++
			continue
		}

		// Check OS compatibility
		if !forceAll && skill.Readiness != nil && !skill.Readiness.OSCompatible {
			excluded++
			continue
		}

		// Check if already synced and up to date
		if skill.Synced {
			// For {baseDir} skills, check if the processed version is current
			if skill.Readiness != nil && skill.Readiness.HasBaseDir {
				sourceMD := filepath.Join(skill.Path, "SKILL.md")
				if isProcessedUpToDate(skill.Name, sourceMD, claudeSkillsDir) {
					skipped++
					continue
				}
				// Needs re-processing — fall through
			} else {
				skipped++
				continue
			}
		}

		// Determine target path
		targetDir := filepath.Join(claudeSkillsDir, skill.Name)

		// Handle {baseDir} skills: create directory with symlinked companions + processed SKILL.md
		if skill.IsDir && skill.Readiness != nil && skill.Readiness.HasBaseDir {
			if pErr := processSkillWithBaseDir(&skill, targetDir); pErr != nil {
				fmt.Fprintf(os.Stderr, "skills: process failed for %s: %v\n", skill.Name, pErr)
				continue
			}
			fmt.Printf("  ✓ %s (processed {baseDir})\n", skill.Name)
			synced++
			continue
		}

		// Standard symlink
		var linkPath string
		if skill.IsDir {
			linkPath = filepath.Join(claudeSkillsDir, skill.Name)
		} else {
			linkPath = filepath.Join(claudeSkillsDir, skill.Name+".md")
		}

		// Remove existing file/link/dir if present
		os.RemoveAll(linkPath)

		if symErr := os.Symlink(skill.Path, linkPath); symErr != nil {
			fmt.Fprintf(os.Stderr, "skills: symlink failed for %s: %v\n", skill.Name, symErr)
			continue
		}

		fmt.Printf("  ✓ %s -> %s\n", skill.Name, skill.Path)
		synced++
	}

	return synced, skipped, excluded, nil
}

// processSkillWithBaseDir creates a skill directory in Claude's skills dir
// with symlinked companion files and a processed SKILL.md (baseDir resolved).
func processSkillWithBaseDir(skill *SkillInfo, targetDir string) error {
	sourceMD := filepath.Join(skill.Path, "SKILL.md")

	// Remove existing target
	os.RemoveAll(targetDir)

	// Create target directory
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		return fmt.Errorf("create dir: %w", err)
	}

	// Symlink all companion files/directories (everything except SKILL.md)
	entries, err := os.ReadDir(skill.Path)
	if err != nil {
		return fmt.Errorf("read source dir: %w", err)
	}
	for _, entry := range entries {
		if entry.Name() == "SKILL.md" {
			continue
		}
		src := filepath.Join(skill.Path, entry.Name())
		dst := filepath.Join(targetDir, entry.Name())
		if err := os.Symlink(src, dst); err != nil {
			return fmt.Errorf("symlink %s: %w", entry.Name(), err)
		}
	}

	// Read and process SKILL.md
	data, err := os.ReadFile(sourceMD)
	if err != nil {
		return fmt.Errorf("read SKILL.md: %w", err)
	}
	processed := resolveBaseDir(string(data), skill.Path)

	// Write processed SKILL.md
	if err := os.WriteFile(filepath.Join(targetDir, "SKILL.md"), []byte(processed), 0644); err != nil {
		return fmt.Errorf("write SKILL.md: %w", err)
	}

	// Write .ccc-processed marker
	sourceHash, _ := hashFile(sourceMD)
	marker := processedMarker{
		SourcePath: sourceMD,
		SourceHash: sourceHash,
		Timestamp:  time.Now().UTC().Format(time.RFC3339),
	}
	markerData, _ := json.Marshal(marker)
	os.WriteFile(filepath.Join(targetDir, ".ccc-processed"), markerData, 0644)

	return nil
}

// isProcessedUpToDate checks if a previously processed skill is still current.
func isProcessedUpToDate(skillName, sourcePath, claudeSkillsDir string) bool {
	markerPath := filepath.Join(claudeSkillsDir, skillName, ".ccc-processed")
	data, err := os.ReadFile(markerPath)
	if err != nil {
		return false
	}

	var marker processedMarker
	if err := json.Unmarshal(data, &marker); err != nil {
		return false
	}

	if marker.SourcePath != sourcePath {
		return false
	}

	currentHash, err := hashFile(sourcePath)
	if err != nil {
		return false
	}

	return marker.SourceHash == currentHash
}

// installSkillFromHub installs a skill from ClawHub using npx.
func installSkillFromHub(slug string) error {
	// Use npx clawdhub to install
	cmd := exec.Command("npx", "clawdhub@latest", "install", slug)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("npx clawdhub install failed: %w", err)
	}

	// After install, sync to Claude Code
	synced, _, _, err := syncSkills(false)
	if err != nil {
		return fmt.Errorf("sync after install failed: %w", err)
	}

	if synced > 0 {
		fmt.Printf("Synced %d new skill(s) to Claude Code\n", synced)
	}

	return nil
}

// searchSkillsHub searches ClawHub registry for skills matching a query.
func searchSkillsHub(query string) error {
	cmd := exec.Command("npx", "clawdhub@latest", "search", query)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// handleSkillsCommand handles the `ccc skills` CLI commands.
func handleSkillsCommand(args []string) {
	if len(args) == 0 {
		// List all skills
		skills := scanSkills()
		if len(skills) == 0 {
			fmt.Println("No skills found.")
			fmt.Println("\nInstall skills with: ccc skills install <slug>")
			fmt.Println("Sync existing skills: ccc skills sync")
			return
		}

		fmt.Printf("Found %d skill(s):\n\n", len(skills))

		// Group by source
		bySource := make(map[string][]SkillInfo)
		for _, s := range skills {
			bySource[s.Source] = append(bySource[s.Source], s)
		}

		for _, source := range []string{"openclaw-bundled", "clawhub", "claude-code"} {
			group := bySource[source]
			if len(group) == 0 {
				continue
			}
			fmt.Printf("  [%s] (%d):\n", source, len(group))
			for _, s := range group {
				syncIcon := "  "
				if s.Synced {
					syncIcon = "✓ "
				}
				kind := "file"
				if s.IsDir {
					kind = "dir "
				}
				fmt.Printf("    %s%-25s %s  %s\n", syncIcon, s.Name, kind, s.Path)
			}
			fmt.Println()
		}

		fmt.Println("✓ = synced to Claude Code (~/.claude/skills/)")
		fmt.Println("\nCommands:")
		fmt.Println("  ccc skills sync [--all]       Sync to Claude Code (--all includes OS-incompatible)")
		fmt.Println("  ccc skills check              Check readiness per skill")
		fmt.Println("  ccc skills install <slug>      Install from ClawHub")
		fmt.Println("  ccc skills install-deps <name> Install missing deps for a skill")
		fmt.Println("  ccc skills search <query>      Search ClawHub")
		return
	}

	switch args[0] {
	case "sync":
		forceAll := false
		for _, a := range args[1:] {
			if a == "--all" || a == "-a" {
				forceAll = true
			}
		}
		synced, skipped, excluded, err := syncSkills(forceAll)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("\nSynced %d, skipped %d (already synced/native)", synced, skipped)
		if excluded > 0 {
			fmt.Printf(", excluded %d (OS-incompatible)", excluded)
		}
		fmt.Println()

	case "check":
		handleSkillsCheck()

	case "install-deps":
		if len(args) < 2 {
			fmt.Println("Usage: ccc skills install-deps <skill-name>")
			os.Exit(1)
		}
		name := args[1]
		fmt.Printf("Installing deps for: %s\n", name)
		actions, err := installSkillDeps(name)
		for _, a := range actions {
			fmt.Printf("  %s\n", a)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("Done.")

	case "install":
		if len(args) < 2 {
			fmt.Println("Usage: ccc skills install <slug>")
			os.Exit(1)
		}
		slug := args[1]
		fmt.Printf("Installing skill: %s\n", slug)
		if err := installSkillFromHub(slug); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Skill '%s' installed and synced.\n", slug)

	case "search":
		if len(args) < 2 {
			fmt.Println("Usage: ccc skills search <query>")
			os.Exit(1)
		}
		if err := searchSkillsHub(strings.Join(args[1:], " ")); err != nil {
			fmt.Fprintf(os.Stderr, "Search failed: %v\n", err)
			os.Exit(1)
		}

	default:
		fmt.Fprintf(os.Stderr, "Unknown skills command: %s\n", args[0])
		fmt.Println("Available: sync, check, install, install-deps, search")
		os.Exit(1)
	}
}

// handleSkillsCheck prints readiness status for all skills.
func handleSkillsCheck() {
	config, _ := loadConfig()
	skills := scanSkills()
	enrichSkills(skills, config)

	ready, needDeps, needConfig, needEnv, osIncompat := 0, 0, 0, 0, 0

	fmt.Printf("Skill Readiness (%d skills, %s):\n\n", len(skills), currentOSLabel())

	for _, s := range skills {
		if s.Readiness == nil {
			continue
		}
		r := s.Readiness

		emoji := ""
		if s.Meta != nil && s.Meta.Metadata != nil && s.Meta.Metadata.Emoji != "" {
			emoji = s.Meta.Metadata.Emoji + " "
		}

		if !r.OSCompatible {
			osNames := ""
			if s.Meta != nil && s.Meta.Metadata != nil {
				osNames = strings.Join(s.Meta.Metadata.OS, ", ")
			}
			fmt.Printf("  SKIP   %s%-22s os: %s only\n", emoji, s.Name, osNames)
			osIncompat++
		} else if len(r.MissingConfig) > 0 {
			fmt.Printf("  CONFIG %s%-22s config: %s\n", emoji, s.Name, strings.Join(r.MissingConfig, ", "))
			needConfig++
		} else if len(r.MissingEnv) > 0 {
			fmt.Printf("  NEEDS  %s%-22s env: %s\n", emoji, s.Name, strings.Join(r.MissingEnv, ", "))
			needEnv++
		} else if len(r.MissingBins) > 0 {
			fmt.Printf("  NEEDS  %s%-22s bins: %s\n", emoji, s.Name, strings.Join(r.MissingBins, ", "))
			needDeps++
		} else {
			detail := ""
			if s.Meta != nil && s.Meta.Metadata != nil && s.Meta.Metadata.Requires != nil {
				if len(s.Meta.Metadata.Requires.Bins) > 0 {
					detail = strings.Join(s.Meta.Metadata.Requires.Bins, ", ")
				}
			}
			if r.AvailableAnyBin != "" {
				detail = r.AvailableAnyBin
			}
			if r.HasBaseDir {
				if detail != "" {
					detail += " "
				}
				detail += "[baseDir]"
			}
			fmt.Printf("  OK     %s%-22s %s\n", emoji, s.Name, detail)
			ready++
		}
	}

	fmt.Printf("\nSummary: %d ready, %d need bins, %d need env, %d need config, %d OS-incompatible\n",
		ready, needDeps, needEnv, needConfig, osIncompat)
}

// currentOSLabel returns a human label for the current OS.
func currentOSLabel() string {
	switch os := strings.ToLower(fmt.Sprintf("%s", getOSName())); os {
	case "linux":
		return "linux"
	case "darwin":
		return "macOS"
	case "windows":
		return "windows"
	default:
		return os
	}
}

func getOSName() string {
	return runtime.GOOS
}
