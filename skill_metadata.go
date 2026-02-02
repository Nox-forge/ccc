package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// SkillFrontmatter holds parsed frontmatter from a SKILL.md file.
type SkillFrontmatter struct {
	Name        string
	Description string
	Homepage    string
	Metadata    *OpenClawMetadata
}

// OpenClawMetadata holds the parsed "openclaw" block from the metadata JSON.
type OpenClawMetadata struct {
	Emoji      string          `json:"emoji"`
	OS         []string        `json:"os"`
	Requires   *SkillRequires  `json:"requires"`
	PrimaryEnv string          `json:"primaryEnv"`
	Install    []InstallMethod `json:"install"`
	SkillKey   string          `json:"skillKey"`
}

// SkillRequires specifies what a skill needs to function.
type SkillRequires struct {
	Bins    []string `json:"bins"`
	AnyBins []string `json:"anyBins"`
	Env     []string `json:"env"`
	Config  []string `json:"config"`
}

// InstallMethod describes one way to install a skill's dependencies.
type InstallMethod struct {
	ID              string   `json:"id"`
	Kind            string   `json:"kind"` // brew, apt, node, go, uv, download
	Formula         string   `json:"formula,omitempty"`
	Cask            string   `json:"cask,omitempty"`
	Package         string   `json:"package,omitempty"`
	Module          string   `json:"module,omitempty"`
	Bins            []string `json:"bins,omitempty"`
	Label           string   `json:"label"`
	OS              []string `json:"os,omitempty"`
	URL             string   `json:"url,omitempty"`
	Archive         string   `json:"archive,omitempty"`
	Extract         bool     `json:"extract,omitempty"`
	StripComponents int      `json:"stripComponents,omitempty"`
	TargetDir       string   `json:"targetDir,omitempty"`
}

// parseFrontmatter parses the YAML-like frontmatter from a SKILL.md file.
// Frontmatter is delimited by --- lines at the top of the file.
// Only 4 keys are expected: name, description, homepage, metadata.
// The metadata value is a JSON string which is parsed into OpenClawMetadata.
func parseFrontmatter(filePath string) (*SkillFrontmatter, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, err
	}

	content := string(data)
	if !strings.HasPrefix(content, "---\n") {
		return nil, fmt.Errorf("no frontmatter found")
	}

	// Find closing ---
	endIdx := strings.Index(content[4:], "\n---")
	if endIdx < 0 {
		return nil, fmt.Errorf("unclosed frontmatter")
	}
	fmBlock := content[4 : 4+endIdx]

	fm := &SkillFrontmatter{}
	var metadataJSON string

	for _, line := range strings.Split(fmBlock, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		colonIdx := strings.Index(line, ":")
		if colonIdx < 0 {
			continue
		}
		key := strings.TrimSpace(line[:colonIdx])
		value := strings.TrimSpace(line[colonIdx+1:])

		switch key {
		case "name":
			fm.Name = value
		case "description":
			fm.Description = value
		case "homepage":
			fm.Homepage = value
		case "metadata":
			metadataJSON = value
		}
	}

	if metadataJSON != "" {
		// The metadata JSON wraps the openclaw object: {"openclaw": {...}}
		var wrapper struct {
			OpenClaw *OpenClawMetadata `json:"openclaw"`
		}
		if err := json.Unmarshal([]byte(metadataJSON), &wrapper); err != nil {
			return fm, nil // return frontmatter without metadata on parse error
		}
		fm.Metadata = wrapper.OpenClaw
	}

	return fm, nil
}

// getSkillBody returns the content after the frontmatter.
func getSkillBody(filePath string) (string, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return "", err
	}
	content := string(data)
	if !strings.HasPrefix(content, "---\n") {
		return content, nil
	}
	endIdx := strings.Index(content[4:], "\n---")
	if endIdx < 0 {
		return content, nil
	}
	// Skip past the closing --- and the newline after it
	bodyStart := 4 + endIdx + 4 // "---\n" = 4 chars
	if bodyStart > len(content) {
		return "", nil
	}
	return content[bodyStart:], nil
}

// hasBaseDir checks if content contains the {baseDir} placeholder.
func hasBaseDir(content string) bool {
	return strings.Contains(content, "{baseDir}")
}

// resolveBaseDir replaces all {baseDir} occurrences with the given directory path.
func resolveBaseDir(content, skillDir string) string {
	return strings.ReplaceAll(content, "{baseDir}", skillDir)
}

// hashFile returns the SHA-256 hex digest of a file's contents.
func hashFile(filePath string) (string, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(data)
	return fmt.Sprintf("%x", h), nil
}
