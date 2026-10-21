package main

import (
	"bufio"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

type ConfigRevision struct {
	Available bool   `json:"available"`
	Commit    string `json:"commit,omitempty"`
	Error     string `json:"error,omitempty"`
}

func readConfigRevision(configDir string) ConfigRevision {
	gitPath := filepath.Join(configDir, ".git")
	info, err := os.Stat(gitPath)
	if errors.Is(err, os.ErrNotExist) {
		return ConfigRevision{}
	}
	if err != nil {
		return ConfigRevision{Available: true, Error: "unable to inspect .git"}
	}

	gitDir := gitPath
	if !info.IsDir() {
		gitFile, readErr := os.ReadFile(gitPath)
		if readErr != nil {
			return ConfigRevision{Available: true, Error: "unable to read .git"}
		}
		line := strings.TrimSpace(string(gitFile))
		if !strings.HasPrefix(line, "gitdir: ") {
			return ConfigRevision{Available: true, Error: "invalid .git file"}
		}
		gitDir = strings.TrimSpace(strings.TrimPrefix(line, "gitdir: "))
		if !filepath.IsAbs(gitDir) {
			gitDir = filepath.Join(configDir, gitDir)
		}
	}

	commonDir := gitDir
	if commonDirFile, readErr := os.ReadFile(filepath.Join(gitDir, "commondir")); readErr == nil {
		commonDir = strings.TrimSpace(string(commonDirFile))
		if !filepath.IsAbs(commonDir) {
			commonDir = filepath.Join(gitDir, commonDir)
		}
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return ConfigRevision{Available: true, Error: "unable to read common Git directory"}
	}

	head, err := os.ReadFile(filepath.Join(gitDir, "HEAD"))
	if err != nil {
		return ConfigRevision{Available: true, Error: "unable to read HEAD"}
	}
	headValue := strings.TrimSpace(string(head))
	if strings.HasPrefix(headValue, "ref: ") {
		ref := strings.TrimSpace(strings.TrimPrefix(headValue, "ref: "))
		if !strings.HasPrefix(ref, "refs/") || !filepath.IsLocal(filepath.FromSlash(ref)) {
			return ConfigRevision{Available: true, Error: "invalid HEAD reference"}
		}
		headValue, err = resolveGitRef([]string{gitDir, commonDir}, ref)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return ConfigRevision{Available: true}
			}
			return ConfigRevision{Available: true, Error: "unable to read HEAD reference"}
		}
	}
	if !validCommitHash(headValue) {
		if headValue == "" {
			return ConfigRevision{Available: true}
		}
		return ConfigRevision{Available: true, Error: "invalid commit hash"}
	}
	return ConfigRevision{Available: true, Commit: strings.ToLower(headValue)}
}

func resolveGitRef(gitDirs []string, ref string) (string, error) {
	refPath := filepath.FromSlash(ref)
	for _, gitDir := range gitDirs {
		value, err := os.ReadFile(filepath.Join(gitDir, refPath))
		if err == nil {
			return strings.TrimSpace(string(value)), nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
	}
	for _, gitDir := range gitDirs {
		value, err := packedRef(gitDir, ref)
		if err == nil {
			return value, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
	}
	return "", os.ErrNotExist
}

func packedRef(gitDir, ref string) (string, error) {
	file, err := os.Open(filepath.Join(gitDir, "packed-refs"))
	if err != nil {
		return "", err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 2 && fields[1] == ref {
			return fields[0], nil
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	return "", os.ErrNotExist
}

func validCommitHash(hash string) bool {
	if len(hash) != 40 && len(hash) != 64 {
		return false
	}
	for _, char := range hash {
		if !(char >= '0' && char <= '9' || char >= 'a' && char <= 'f' || char >= 'A' && char <= 'F') {
			return false
		}
	}
	return true
}
