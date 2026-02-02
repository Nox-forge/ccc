package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// installSkillDeps installs missing dependencies for a named skill.
// Returns the list of actions taken and any error.
func installSkillDeps(skillName string) ([]string, error) {
	skills := scanSkills()

	var skill *SkillInfo
	for i := range skills {
		if skills[i].Name == skillName {
			skill = &skills[i]
			break
		}
	}
	if skill == nil {
		return nil, fmt.Errorf("skill %q not found", skillName)
	}

	// Parse metadata if not already done
	if skill.Meta == nil {
		skillMD := skill.Path
		if skill.IsDir {
			skillMD = filepath.Join(skill.Path, "SKILL.md")
		}
		fm, err := parseFrontmatter(skillMD)
		if err != nil {
			return nil, fmt.Errorf("parse frontmatter: %w", err)
		}
		skill.Meta = fm
	}

	if skill.Meta.Metadata == nil {
		return nil, fmt.Errorf("skill %q has no metadata", skillName)
	}

	meta := skill.Meta.Metadata
	if len(meta.Install) == 0 {
		return nil, fmt.Errorf("skill %q has no install methods defined", skillName)
	}

	methods := selectInstallMethods(meta.Install)
	if len(methods) == 0 {
		return nil, fmt.Errorf("no install methods available for %s on %s", skillName, runtime.GOOS)
	}

	var actions []string
	for _, m := range methods {
		// Check if the binaries this method provides are already installed
		allPresent := true
		for _, bin := range m.Bins {
			if _, err := exec.LookPath(bin); err != nil {
				allPresent = false
				break
			}
		}
		if allPresent && len(m.Bins) > 0 {
			actions = append(actions, fmt.Sprintf("skip %s: binaries already installed", m.Label))
			continue
		}

		var err error
		switch m.Kind {
		case "apt":
			err = installApt(m)
		case "brew":
			err = installBrew(m)
		case "node":
			err = installNode(m)
		case "go":
			err = installGo(m)
		case "uv":
			err = installUV(m)
		case "download":
			err = installDownload(m)
		default:
			actions = append(actions, fmt.Sprintf("skip %s: unknown kind %q", m.Label, m.Kind))
			continue
		}

		if err != nil {
			return actions, fmt.Errorf("%s failed: %w", m.Label, err)
		}
		actions = append(actions, fmt.Sprintf("installed: %s", m.Label))
	}

	return actions, nil
}

// selectInstallMethods filters install methods for the current OS
// and prefers apt on Linux over brew.
func selectInstallMethods(methods []InstallMethod) []InstallMethod {
	currentOS := runtime.GOOS

	var applicable []InstallMethod
	for _, m := range methods {
		// If method specifies OS, check compatibility
		if len(m.OS) > 0 {
			compatible := false
			for _, os := range m.OS {
				if mapOSName(os) == currentOS {
					compatible = true
					break
				}
			}
			if !compatible {
				continue
			}
		}

		// On Linux, skip brew if apt is available for the same bins
		if currentOS == "linux" && m.Kind == "brew" {
			// Check if there's an apt or node alternative
			hasAlt := false
			for _, other := range methods {
				if other.Kind == "apt" || other.Kind == "node" || other.Kind == "go" || other.Kind == "uv" {
					// Check if they install the same bins
					if sameBins(m.Bins, other.Bins) {
						hasAlt = true
						break
					}
				}
			}
			if hasAlt {
				continue
			}
		}

		applicable = append(applicable, m)
	}

	return applicable
}

// sameBins checks if two bin lists overlap.
func sameBins(a, b []string) bool {
	for _, x := range a {
		for _, y := range b {
			if x == y {
				return true
			}
		}
	}
	return false
}

func installApt(m InstallMethod) error {
	formula := m.Formula
	if formula == "" {
		formula = m.Package
	}
	if formula == "" {
		return fmt.Errorf("no formula/package specified")
	}
	fmt.Printf("  apt: installing %s...\n", formula)
	cmd := exec.Command("sudo", "apt-get", "install", "-y", formula)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func installBrew(m InstallMethod) error {
	if _, err := exec.LookPath("brew"); err != nil {
		return fmt.Errorf("brew not found in PATH")
	}
	if m.Cask != "" {
		fmt.Printf("  brew: installing cask %s...\n", m.Cask)
		cmd := exec.Command("brew", "install", "--cask", m.Cask)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd.Run()
	}
	formula := m.Formula
	if formula == "" {
		return fmt.Errorf("no formula specified")
	}
	fmt.Printf("  brew: installing %s...\n", formula)
	cmd := exec.Command("brew", "install", formula)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func installNode(m InstallMethod) error {
	pkg := m.Package
	if pkg == "" {
		return fmt.Errorf("no package specified")
	}
	fmt.Printf("  npm: installing %s globally...\n", pkg)
	cmd := exec.Command("npm", "install", "-g", pkg)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func installGo(m InstallMethod) error {
	mod := m.Module
	if mod == "" {
		return fmt.Errorf("no module specified")
	}
	fmt.Printf("  go: installing %s...\n", mod)
	cmd := exec.Command("go", "install", mod+"@latest")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func installUV(m InstallMethod) error {
	pkg := m.Package
	if pkg == "" {
		return fmt.Errorf("no package specified")
	}
	if _, err := exec.LookPath("uv"); err != nil {
		return fmt.Errorf("uv not found in PATH")
	}
	fmt.Printf("  uv: installing %s...\n", pkg)
	cmd := exec.Command("uv", "tool", "install", pkg)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func installDownload(m InstallMethod) error {
	if m.URL == "" {
		return fmt.Errorf("no URL specified")
	}
	targetDir := m.TargetDir
	if targetDir == "" {
		return fmt.Errorf("no targetDir specified")
	}
	targetDir = expandPath(targetDir)

	fmt.Printf("  download: %s -> %s\n", m.URL, targetDir)

	if err := os.MkdirAll(targetDir, 0755); err != nil {
		return fmt.Errorf("create target dir: %w", err)
	}

	if m.Extract && m.Archive != "" {
		// Download and extract
		tmpFile := filepath.Join(os.TempDir(), "ccc-download-"+m.ID)
		defer os.Remove(tmpFile)

		// Download with curl
		dlCmd := exec.Command("curl", "-fSL", "-o", tmpFile, m.URL)
		dlCmd.Stderr = os.Stderr
		if err := dlCmd.Run(); err != nil {
			return fmt.Errorf("download failed: %w", err)
		}

		// Extract based on archive type
		switch {
		case strings.Contains(m.Archive, "tar"):
			args := []string{"-xf", tmpFile, "-C", targetDir}
			if m.StripComponents > 0 {
				args = append(args, fmt.Sprintf("--strip-components=%d", m.StripComponents))
			}
			exCmd := exec.Command("tar", args...)
			exCmd.Stderr = os.Stderr
			if err := exCmd.Run(); err != nil {
				return fmt.Errorf("extract failed: %w", err)
			}
		case m.Archive == "zip":
			exCmd := exec.Command("unzip", "-o", tmpFile, "-d", targetDir)
			exCmd.Stderr = os.Stderr
			if err := exCmd.Run(); err != nil {
				return fmt.Errorf("extract failed: %w", err)
			}
		default:
			return fmt.Errorf("unknown archive type: %s", m.Archive)
		}
	} else {
		// Simple download
		dlCmd := exec.Command("curl", "-fSL", "-o", filepath.Join(targetDir, filepath.Base(m.URL)), m.URL)
		dlCmd.Stderr = os.Stderr
		if err := dlCmd.Run(); err != nil {
			return fmt.Errorf("download failed: %w", err)
		}
	}

	return nil
}
