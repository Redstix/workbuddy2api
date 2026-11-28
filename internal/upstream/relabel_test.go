package upstream

import (
	"encoding/json"
	"strings"
	"testing"
)

// decodeMessages 把出站 body 解成 messages 切片，便于逐条断言。
func decodeMessages(t *testing.T, body []byte) []map[string]any {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	raw, ok := obj["messages"].([]any)
	if !ok {
		t.Fatalf("messages missing or wrong type: %T", obj["messages"])
	}
	out := make([]map[string]any, 0, len(raw))
	for i, m := range raw {
		mm, ok := m.(map[string]any)
		if !ok {
			t.Fatalf("message %d is not an object", i)
		}
		out = append(out, mm)
	}
	return out
}

func mustBody(t *testing.T, msgs []map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{"model": "deepseek-v4.1-flash", "messages": msgs})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// ---- RelabelSystemToUser ----

func TestRelabelSingleSystem(t *testing.T) {
	body := mustBody(t, []map[string]any{
		{"role": "system", "content": "You are an AI agent."},
		{"role": "user", "content": "hi"},
	})
	out := RelabelSystemToUser(body)
	msgs := decodeMessages(t, out)
	if got := msgs[0]["role"]; got != "user" {
		t.Fatalf("first role = %v, want user", got)
	}
	if got := msgs[0]["content"]; got != "You are an AI agent." {
		t.Fatalf("content changed: %v", got)
	}
	if len(msgs) != 2 {
		t.Fatalf("message count changed: %d", len(msgs))
	}
}

func TestRelabelSystemAndDeveloper(t *testing.T) {
	body := mustBody(t, []map[string]any{
		{"role": "system", "content": "a"},
		{"role": "developer", "content": "b"},
		{"role": "user", "content": "c"},
	})
	msgs := decodeMessages(t, RelabelSystemToUser(body))
	for i := 0; i < 2; i++ {
		if msgs[i]["role"] != "user" {
			t.Fatalf("msg %d role = %v, want user", i, msgs[i]["role"])
		}
	}
}

func TestRelabelRoleVariants(t *testing.T) {
	// 大小写与首尾空格变体都要覆盖。
	for _, role := range []string{"System", "SYSTEM", " developer ", "Developer"} {
		body := mustBody(t, []map[string]any{{"role": role, "content": "x"}})
		msgs := decodeMessages(t, RelabelSystemToUser(body))
		if msgs[0]["role"] != "user" {
			t.Fatalf("role %q not relabeled: %v", role, msgs[0]["role"])
		}
	}
}

func TestRelabelPreservesOrderAndCount(t *testing.T) {
	in := []map[string]any{
		{"role": "system", "content": "s"},
		{"role": "user", "content": "u1"},
		{"role": "assistant", "content": "a1"},
		{"role": "user", "content": "u2"},
		{"role": "assistant", "content": "a2"},
	}
	out := decodeMessages(t, RelabelSystemToUser(mustBody(t, in)))
	if len(out) != len(in) {
		t.Fatalf("count = %d, want %d", len(out), len(in))
	}
	wantContent := []string{"s", "u1", "a1", "u2", "a2"}
	wantRole := []string{"user", "user", "assistant", "user", "assistant"}
	for i := range in {
		if out[i]["content"] != wantContent[i] {
			t.Fatalf("msg %d content = %v, want %q (order must not change)", i, out[i]["content"], wantContent[i])
		}
		if out[i]["role"] != wantRole[i] {
			t.Fatalf("msg %d role = %v, want %q", i, out[i]["role"], wantRole[i])
		}
	}
}

func TestRelabelKeepsMultimodalContent(t *testing.T) {
	parts := []any{
		map[string]any{"type": "text", "text": "hello"},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,AAA"}},
	}
	body, err := json.Marshal(map[string]any{"messages": []any{
		map[string]any{"role": "system", "content": parts},
	}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	msgs := decodeMessages(t, RelabelSystemToUser(body))
	if msgs[0]["role"] != "user" {
		t.Fatalf("role = %v", msgs[0]["role"])
	}
	gotParts, ok := msgs[0]["content"].([]any)
	if !ok {
		t.Fatalf("multimodal content lost: %T", msgs[0]["content"])
	}
	if len(gotParts) != 2 {
		t.Fatalf("part count = %d, want 2", len(gotParts))
	}
}

func TestRelabelBadInputsReturnedUnchanged(t *testing.T) {
	cases := map[string][]byte{
		"empty":              {},
		"invalid json":       []byte("{not json"),
		"no messages":        []byte(`{"model":"x"}`),
		"empty msgs":         []byte(`{"messages":[]}`),
		"messages not array": []byte(`{"messages":"nope"}`),
	}
	for name, body := range cases {
		out := RelabelSystemToUser(body)
		if string(out) != string(body) {
			t.Fatalf("%s: body was modified", name)
		}
	}
}

func TestRelabelNoSystemIsNoop(t *testing.T) {
	body := mustBody(t, []map[string]any{
		{"role": "user", "content": "u"},
		{"role": "assistant", "content": "a"},
	})
	if out := RelabelSystemToUser(body); string(out) != string(body) {
		t.Fatalf("body changed although nothing to relabel")
	}
}

func TestRelabelIdempotent(t *testing.T) {
	body := mustBody(t, []map[string]any{
		{"role": "system", "content": "s"},
		{"role": "user", "content": "u"},
	})
	once := RelabelSystemToUser(body)
	twice := RelabelSystemToUser(once)
	if string(once) != string(twice) {
		t.Fatalf("not idempotent:\n once=%s\n twice=%s", once, twice)
	}
}

// ---- PrependNeutralSystem ----

func TestPrependNeutralSystem(t *testing.T) {
	body := mustBody(t, []map[string]any{
		{"role": "user", "content": "u"},
	})
	msgs := decodeMessages(t, PrependNeutralSystem(body))
	if len(msgs) != 2 {
		t.Fatalf("count = %d, want 2", len(msgs))
	}
	if msgs[0]["role"] != "system" {
		t.Fatalf("head role = %v, want system", msgs[0]["role"])
	}
	if msgs[0]["content"] != neutralSystemContent {
		t.Fatalf("head content = %v", msgs[0]["content"])
	}
	if msgs[1]["content"] != "u" {
		t.Fatalf("original message displaced: %v", msgs[1]["content"])
	}
}

func TestPrependNeutralSystemIsIdempotent(t *testing.T) {
	body := mustBody(t, []map[string]any{{"role": "user", "content": "u"}})
	once := PrependNeutralSystem(body)
	twice := PrependNeutralSystem(once)
	if string(once) != string(twice) {
		t.Fatalf("not idempotent — head would be duplicated")
	}
	msgs := decodeMessages(t, twice)
	if len(msgs) != 2 {
		t.Fatalf("count = %d, want 2 (no duplicate head)", len(msgs))
	}
}

func TestPrependNeutralSystemBadInputsUnchanged(t *testing.T) {
	for _, body := range [][]byte{{}, []byte("{bad"), []byte(`{"messages":[]}`)} {
		if out := PrependNeutralSystem(body); string(out) != string(body) {
			t.Fatalf("body modified: %q", out)
		}
	}
}

// ---- NormalizeSystemRoles（两步串联，最关键的一组） ----

// 完整形态断言：输入 [system:A, user:B, assistant:C]
// 输出必须是 [system:"You are a helpful assistant.", user:A, user:B, assistant:C]，
// 条数 +1，第 2 条 content 与 A 逐字节相同。
func TestNormalizeSystemRolesFullShape(t *testing.T) {
	const originalSystem = "You are an AI agent powered by DeepSeek Harness."
	body := mustBody(t, []map[string]any{
		{"role": "system", "content": originalSystem},
		{"role": "user", "content": "B"},
		{"role": "assistant", "content": "C"},
	})
	msgs := decodeMessages(t, NormalizeSystemRoles(body))

	if len(msgs) != 4 {
		t.Fatalf("count = %d, want 4", len(msgs))
	}
	if msgs[0]["role"] != "system" || msgs[0]["content"] != neutralSystemContent {
		t.Fatalf("head = %v/%v, want system/%q", msgs[0]["role"], msgs[0]["content"], neutralSystemContent)
	}
	if msgs[1]["role"] != "user" {
		t.Fatalf("msg1 role = %v, want user", msgs[1]["role"])
	}
	if msgs[1]["content"] != originalSystem {
		t.Fatalf("msg1 content = %v, want original system byte-identical", msgs[1]["content"])
	}
	if msgs[2]["content"] != "B" || msgs[3]["content"] != "C" {
		t.Fatalf("tail order changed: %v / %v", msgs[2]["content"], msgs[3]["content"])
	}
}

// 出站 body 里除了恒定的一条中性头之外，不得残留任何 system/developer 角色。
func TestNormalizeSystemRolesLeavesNoSystemExceptHead(t *testing.T) {
	body := mustBody(t, []map[string]any{
		{"role": "system", "content": "s1"},
		{"role": "developer", "content": "s2"},
		{"role": "user", "content": "u"},
	})
	out := NormalizeSystemRoles(body)
	msgs := decodeMessages(t, out)
	if msgs[0]["role"] != "system" {
		t.Fatalf("head role = %v, want system", msgs[0]["role"])
	}
	for i := 1; i < len(msgs); i++ {
		role, _ := msgs[i]["role"].(string)
		if strings.EqualFold(role, "system") || strings.EqualFold(role, "developer") {
			t.Fatalf("msg %d still has blocked role %q", i, role)
		}
	}
}

func TestNormalizeSystemRolesNoSystemInput(t *testing.T) {
	// 客户端完全不带 system：仍然要插中性头，保证 global 首条是 system。
	body := mustBody(t, []map[string]any{
		{"role": "user", "content": "u"},
	})
	msgs := decodeMessages(t, NormalizeSystemRoles(body))
	if len(msgs) != 2 {
		t.Fatalf("count = %d, want 2", len(msgs))
	}
	if msgs[0]["role"] != "system" || msgs[0]["content"] != neutralSystemContent {
		t.Fatalf("head = %v/%v", msgs[0]["role"], msgs[0]["content"])
	}
}

func TestNormalizeSystemRolesIdempotent(t *testing.T) {
	body := mustBody(t, []map[string]any{
		{"role": "system", "content": "s"},
		{"role": "user", "content": "u"},
	})
	once := NormalizeSystemRoles(body)
	twice := NormalizeSystemRoles(once)
	if string(once) != string(twice) {
		t.Fatalf("not idempotent:\n once=%s\n twice=%s", once, twice)
	}
}

func TestNormalizeSystemRolesBadInputUnchanged(t *testing.T) {
	for _, body := range [][]byte{{}, []byte("{bad"), []byte(`{}`)} {
		if out := NormalizeSystemRoles(body); string(out) != string(body) {
			t.Fatalf("body modified: %q", out)
		}
	}
}
