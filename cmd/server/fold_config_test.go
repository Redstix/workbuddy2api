package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestFoldSystemMessagesConfig 配置项 features.fold_system_messages：
// 默认 false（不改存量行为），显式 true 生效，JSON 与 env 两条入口都认。
func TestFoldSystemMessagesConfigDefault(t *testing.T) {
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.Features.FoldSystemMessages {
		t.Error("features.fold_system_messages must default to false")
	}
	if !c.Features.SanitizeBlacklistFingerprints {
		t.Error("sanitize_blacklist_fingerprints must stay true by default (unchanged)")
	}
}

// TestFoldSystemMessagesConfigJSON 显式 JSON 开关。
func TestFoldSystemMessagesConfigJSON(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"features":{"fold_system_messages":true}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Features.FoldSystemMessages {
		t.Error("features.fold_system_messages=true not applied")
	}
}

// TestFoldSystemMessagesConfigEnv WB2A_FOLD_SYSTEM_MESSAGES 环境变量入口。
func TestFoldSystemMessagesConfigEnv(t *testing.T) {
	t.Setenv("WB2A_FOLD_SYSTEM_MESSAGES", "true")
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if !c.Features.FoldSystemMessages {
		t.Error("WB2A_FOLD_SYSTEM_MESSAGES=true not applied")
	}

	t.Setenv("WB2A_FOLD_SYSTEM_MESSAGES", "false")
	c, err = Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.Features.FoldSystemMessages {
		t.Error("WB2A_FOLD_SYSTEM_MESSAGES=false not applied")
	}
}
