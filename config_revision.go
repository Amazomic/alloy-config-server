package main

import (
	"bufio"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type ConfigRevision struct {
	Available bool   `json:"available"`
	Commit    string `json:"commit,omitempty"`
	URL       string `json:"url,omitempty"`
	Error     string `json:"error,omitempty"`
}

func readConfigRevision(configDir string) ConfigRevision {
	gitPath := filepath.Join(configDir, ".git")
	info, err := os.Stat(gitPath)
	if errors.Is(err, os.ErrNotExist) {
		// An explicit metadata mount may point directly to a Git directory.
		if head, headErr := os.Stat(filepath.Join(configDir, "HEAD")); headErr != nil || head.IsDir() {
			return ConfigRevision{}
		}
		if config, configErr := os.Stat(filepath.Join(configDir, "config")); configErr != nil || config.IsDir() {
			return ConfigRevision{}
		}
		gitPath = configDir
		info, err = os.Stat(gitPath)
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
	commit := strings.ToLower(headValue)
	return ConfigRevision{Available: true, Commit: commit, URL: configCommitURL(commonDir, commit)}
}

// Read only origin's URL; never execute Git or follow config includes.
func configCommitURL(gitDir, commit string) string {
	file, err := os.Open(filepath.Join(gitDir, "config"))
	if err != nil {
		return ""
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	origin := false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "[") {
			origin = strings.EqualFold(line, `[remote "origin"]`)
			continue
		}
		if !origin {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || !strings.EqualFold(strings.TrimSpace(key), "url") {
			continue
		}
		value = strings.TrimSpace(value)
		if strings.HasPrefix(value, `"`) {
			value, err = strconv.Unquote(value)
			if err != nil {
				return ""
			}
		}
		return commitWebURL(value, commit)
	}
	return ""
}

func commitWebURL(remote, commit string) string {
	if !validCommitHash(commit) {
		return ""
	}
	if !strings.Contains(remote, "://") {
		// Git's SCP-like SSH syntax: git@host:owner/repository.git.
		host, repo, ok := strings.Cut(remote, ":")
		if !ok || !strings.Contains(host, "@") {
			return ""
		}
		remote = "ssh://" + host + "/" + repo
	}
	u, err := url.Parse(remote)
	if err != nil || u.Hostname() == "" {
		return ""
	}
	switch u.Scheme {
	case "ssh", "git":
		u.Scheme, u.Host = "https", u.Hostname()
	case "http", "https":
	default:
		return ""
	}
	u.User, u.RawQuery, u.Fragment = nil, "", ""
	u.RawPath = ""
	u.Path = strings.TrimSuffix(strings.TrimRight(u.Path, "/"), ".git")
	if u.Path == "" || u.Path == "/" {
		return ""
	}
	commitPath := "/commit/"
	if strings.Contains(strings.ToLower(u.Hostname()), "gitlab") {
		commitPath = "/-/commit/"
	} else if strings.EqualFold(u.Hostname(), "bitbucket.org") {
		commitPath = "/commits/"
	}
	u.Path += commitPath + commit
	return u.String()
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
