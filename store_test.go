package main

import (
	"strings"
	"testing"
)

func TestMergeOmitsFileNames(t *testing.T) {
	cfgs := []*Config{
		{Name: "env_prod", content: "// User comment\nlogging { level = \"{{name}}\" }\n"},
		{Name: "remote_local", content: "prometheus.exporter.unix \"host\" {}\n"},
	}
	content, hash := MergeForAttributes(cfgs, map[string]string{"name": "info"})
	want := "declare \"node_stack\" {\n// User comment\nlogging { level = \"info\" }\n\nprometheus.exporter.unix \"host\" {}\n\n}\n\nnode_stack \"default\" { }\n"
	if content != want {
		t.Fatalf("merged content = %q, want %q", content, want)
	}
	if hash != sha(content) {
		t.Fatal("hash does not match final content")
	}
	for _, cfg := range cfgs {
		if strings.Contains(content, cfg.Name) {
			t.Fatalf("file name %q leaked into final config", cfg.Name)
		}
		cfg.Name += "_renamed"
	}
	if renamed, renamedHash := MergeForAttributes(cfgs, map[string]string{"name": "info"}); renamed != content || renamedHash != hash {
		t.Fatal("renaming source files changed final content or hash")
	}
}

func TestConfigPartsPreserveFinalContent(t *testing.T) {
	cfgs := []*Config{
		{Name: "env_prod", Selector: map[string]string{"env": "prod"}, content: "// Привет\nlogging { level = \"{{name}}\" }\n"},
		{Name: "remote_local", Selector: map[string]string{"remote": "local"}, content: "// second block\n"},
	}
	attrs := map[string]string{"name": "info"}
	content, hash, parts := mergeConfigParts(cfgs, attrs)
	wantContent, wantHash := MergeForAttributes(cfgs, attrs)
	if content != wantContent || hash != wantHash || len(parts) != 4 {
		t.Fatal("display metadata changed final config or hash")
	}
	var reconstructed strings.Builder
	for _, part := range parts {
		reconstructed.WriteString(part.Content)
	}
	if reconstructed.String() != content || parts[1].Name != "env_prod" || parts[1].Selector["env"] != "prod" {
		t.Fatal("parts do not describe the final configuration")
	}
	if strings.Contains(parts[1].Content, "{{name}}") {
		t.Fatal("displayed part was not rendered for this collector")
	}
	cfgs[0].Selector["env"] = "changed"
	if parts[1].Selector["env"] != "prod" {
		t.Fatal("source changes modified saved display metadata")
	}
}
