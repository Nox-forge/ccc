package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// SkillInfo describes a discovered skill.
type SkillInfo struct {
	Name   string // filename without .md extension
	Path   string // absolute path to the skill file
	Source string // "clawhub", "openclaw-bundled", "claude-code", "ccc"
	Synced bool   // true if symlinked into ~/.claude/skills/
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
		filepath.Join(home, ".openclaw", "tools", "skills"),
		filepath.Join(home, "node_modules", "openclaw", "skills"),
		filepath.Join(home, "node_modules", "@openclaw", "core", "skills"),
	}

	// Also search within .openclaw directory for any node_modules skills
	ocNodeModules := filepath.Join(home, ".openclaw", "node_modules")
	if info, err := os.Stat(ocNodeModules); err == nil && info.IsDir() {
		// Look for skills directories within openclaw packages
		entries, _ := os.ReadDir(ocNodeModules)
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), "openclaw") || strings.HasPrefix(e.Name(), "@openclaw") {
				skillsDir := filepath.Join(ocNodeModules, e.Name(), "skills")
				candidates = append(candidates, skillsDir)
			}
		}
	}

	for _, dir := range candidates {
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			return dir
		}
	}
	return ""
}

// scanSkills discovers all available skills across all sources.
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
			if !strings.HasSuffix(name, ".md") {
				continue
			}

			baseName := strings.TrimSuffix(name, ".md")
			if seen[baseName] {
				continue
			}
			seen[baseName] = true

			fullPath := filepath.Join(dir, name)

			// Check if it's synced (symlinked into claude-code skills dir)
			synced := false
			claudePath := filepath.Join(claudeSkillsDir, name)
			if target, err := os.Readlink(claudePath); err == nil {
				synced = target == fullPath
			} else if _, err := os.Stat(claudePath); err == nil && source == "claude-code" {
				synced = true // Native claude-code skill
			}

			skills = append(skills, SkillInfo{
				Name:   baseName,
				Path:   fullPath,
				Source: source,
				Synced: synced,
			})
		}
	}

	sort.Slice(skills, func(i, j int) bool {
		return skills[i].Name < skills[j].Name
	})

	return skills
}

// syncSkills creates symlinks for all non-claude-code skills into ~/.claude/skills/.
func syncSkills() (int, int, error) {
	home, _ := os.UserHomeDir()
	claudeSkillsDir := filepath.Join(home, ".claude", "skills")

	if err := os.MkdirAll(claudeSkillsDir, 0755); err != nil {
		return 0, 0, fmt.Errorf("create skills dir: %w", err)
	}

	skills := scanSkills()
	synced := 0
	skipped := 0

	for _, skill := range skills {
		if skill.Source == "claude-code" {
			skipped++
			continue // Already in the right place
		}
		if skill.Synced {
			skipped++
			continue // Already symlinked
		}

		linkPath := filepath.Join(claudeSkillsDir, skill.Name+".md")

		// Remove existing file/link if present
		os.Remove(linkPath)

		if err := os.Symlink(skill.Path, linkPath); err != nil {
			fmt.Fprintf(os.Stderr, "skills: symlink failed for %s: %v\n", skill.Name, err)
			continue
		}

		synced++
	}

	return synced, skipped, nil
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
	synced, _, err := syncSkills()
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
		for _, s := range skills {
			syncIcon := "  "
			if s.Synced {
				syncIcon = "✓ "
			}
			fmt.Printf("  %s%-30s [%s] %s\n", syncIcon, s.Name, s.Source, s.Path)
		}

		fmt.Println("\n✓ = synced to Claude Code (~/.claude/skills/)")
		fmt.Println("\nCommands:")
		fmt.Println("  ccc skills sync          Sync all to Claude Code")
		fmt.Println("  ccc skills install <slug> Install from ClawHub")
		fmt.Println("  ccc skills search <query> Search ClawHub")
		return
	}

	switch args[0] {
	case "sync":
		synced, skipped, err := syncSkills()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Synced %d skill(s), skipped %d (already synced or native)\n", synced, skipped)

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
		fmt.Println("Available: sync, install, search")
		os.Exit(1)
	}
}
