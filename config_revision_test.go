package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const revisionTestCommit = "0123456789012345678901234567890123456789"

func writeRevisionFixture(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestConfigRevisionLayouts(t *testing.T) {
	for _, layout := range []struct {
		name  string
		files map[string]string
		dir   string
	}{
		{"clone", map[string]string{
			"configs/.git/HEAD":            "ref: refs/heads/main\n",
			"configs/.git/refs/heads/main": revisionTestCommit + "\n",
			"configs/.git/config":          "[remote \"origin\"]\n\turl = git@github.com:example/templates.git\n",
		}, "configs"},
		{"submodule", map[string]string{
			"configs/.git":    "gitdir: ../metadata\n",
			"metadata/HEAD":   revisionTestCommit + "\n",
			"metadata/config": "[remote \"origin\"]\nurl = https://github.com/example/templates.git\n",
		}, "configs"},
		{"worktree", map[string]string{
			"configs/.git":       "gitdir: ../worktree\n",
			"worktree/HEAD":      "ref: refs/heads/release\n",
			"worktree/commondir": "../common\n",
			"common/packed-refs": revisionTestCommit + " refs/heads/release\n",
			"common/config":      "[remote \"origin\"]\nurl = https://github.com/example/templates\n",
		}, "configs"},
		{"explicit-metadata-mount", map[string]string{
			"metadata/HEAD":   revisionTestCommit + "\n",
			"metadata/config": "[remote \"origin\"]\nurl = https://github.com/example/templates\n",
		}, "metadata"},
	} {
		t.Run(layout.name, func(t *testing.T) {
			root := t.TempDir()
			writeRevisionFixture(t, root, layout.files)
			revision := readConfigRevision(filepath.Join(root, layout.dir))
			if !revision.Available || revision.Error != "" || revision.Commit != revisionTestCommit || revision.URL != "https://github.com/example/templates/commit/"+revisionTestCommit {
				t.Fatalf("unexpected revision: %#v", revision)
			}
		})
	}
}

func TestConfigCommitLinks(t *testing.T) {
	for _, test := range []struct{ remote, want string }{
		{"git@github.com:example/repo.git", "https://github.com/example/repo/commit/"},
		{"ssh://git@github.example:2222/team/repo.git", "https://github.example/team/repo/commit/"},
		{"https://user:secret@github.com/team/repo.git?token=secret#fragment", "https://github.com/team/repo/commit/"},
		{"git@gitlab.com:team/repo.git", "https://gitlab.com/team/repo/-/commit/"},
		{"https://bitbucket.org/team/repo.git", "https://bitbucket.org/team/repo/commits/"},
	} {
		if got := commitWebURL(test.remote, revisionTestCommit); got != test.want+revisionTestCommit {
			t.Errorf("commit URL = %q, want %q", got, test.want+revisionTestCommit)
		}
	}
	for _, remote := range []string{"javascript:alert(1)", "file:///tmp/repo", "/tmp/repo", "https://github.com/", "https://"} {
		if got := commitWebURL(remote, revisionTestCommit); got != "" {
			t.Errorf("unsafe/unsupported remote produced link %q", got)
		}
	}
}

func TestConfigRevisionWithoutGitOrRemote(t *testing.T) {
	root := t.TempDir()
	if got := readConfigRevision(root); got.Available || got.Commit != "" {
		t.Fatalf("ordinary directory has Git revision: %#v", got)
	}
	writeRevisionFixture(t, root, map[string]string{".git/HEAD": revisionTestCommit + "\n"})
	if got := readConfigRevision(root); got.Commit != revisionTestCommit || got.URL != "" {
		t.Fatalf("missing origin should retain hash without link: %#v", got)
	}
	writeRevisionFixture(t, root, map[string]string{".git/HEAD": "ref: refs/../../outside\n"})
	if got := readConfigRevision(root); got.Commit != "" || !strings.Contains(got.Error, "invalid") {
		t.Fatalf("unsafe ref accepted: %#v", got)
	}
}
