package upstream

import (
	"encoding/json"
	"strings"
	"testing"
)

// foldRoles 提取 messages 的 role 序列，供断言折叠后的形状。
func foldRoles(t *testing.T, body []byte) []string {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, body)
	}
	msgs, ok := obj["messages"].([]any)
	if !ok {
		t.Fatalf("messages not []any: %v", obj["messages"])
	}
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			t.Fatalf("msg not map: %v", m)
		}
		r, _ := mm["role"].(string)
		out = append(out, r)
	}
	return out
}

// foldFirstUser 取折叠后首条 user 消息的 content（string 形态）。
func foldFirstUser(t *testing.T, body []byte) string {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, body)
	}
	msgs := obj["messages"].([]any)
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			continue
		}
		if role, _ := mm["role"].(string); role == "user" {
			s, _ := mm["content"].(string)
			return s
		}
	}
	t.Fatalf("no user message in %s", body)
	return ""
}

// TestFoldSystemMessagesBasic F1 核心：system 内容前置合并进首条 user，
// system 消息消失，其余消息逐字保留、顺序不变。
func TestFoldSystemMessagesBasic(t *testing.T) {
	in := []byte(`{
		"model":"glm-5.2",
		"stream":true,
		"messages":[
			{"role":"system","content":"你是提取器，只输出 JSON"},
			{"role":"user","content":"把这段对话写成记忆"}
		],
		"temperature":0.3
	}`)
	out := FoldSystemMessages(in)
	got := foldRoles(t, out)
	want := []string{"user"}
	if len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("roles=%v want %v (body=%s)", got, want, out)
	}
	content := foldFirstUser(t, out)
	if !strings.Contains(content, "你是提取器，只输出 JSON") {
		t.Errorf("system text lost: %q", content)
	}
	if !strings.HasSuffix(content, "把这段对话写成记忆") {
		t.Errorf("user text must stay at tail: %q", content)
	}
	if !strings.Contains(content, "你是提取器，只输出 JSON\n\n把这段对话写成记忆") {
		t.Errorf("fold must precede original text with blank line: %q", content)
	}
	// 其余字段零改动。
	var obj map[string]any
	json.Unmarshal(out, &obj)
	if obj["model"] != "glm-5.2" || obj["stream"] != true || obj["temperature"] != 0.3 {
		t.Errorf("other fields changed: %v", obj)
	}
}

// TestFoldSystemMessagesMultipleAndDeveloper F2：多条 system + developer 按原顺序
// 折叠，块间空行分隔。
func TestFoldSystemMessagesMultipleAndDeveloper(t *testing.T) {
	in := []byte(`{"messages":[
		{"role":"system","content":"第一条"},
		{"role":"developer","content":"第二条"},
		{"role":"system","content":"第三条"},
		{"role":"user","content":"问题"}
	]}`)
	out := FoldSystemMessages(in)
	if got := foldRoles(t, out); len(got) != 1 || got[0] != "user" {
		t.Fatalf("roles=%v", got)
	}
	content := foldFirstUser(t, out)
	want := "第一条\n\n第二条\n\n第三条\n\n问题"
	if content != want {
		t.Errorf("content=%q want %q", content, want)
	}
}

// TestFoldSystemMessagesOnlyFirstUser F3：多轮历史只在**首条** user 注入一次，
// 后续 user/assistant 轮次原样不动。
func TestFoldSystemMessagesOnlyFirstUser(t *testing.T) {
	in := []byte(`{"messages":[
		{"role":"system","content":"SYS"},
		{"role":"user","content":"第一问"},
		{"role":"assistant","content":"第一答"},
		{"role":"user","content":"第二问"}
	]}`)
	out := FoldSystemMessages(in)
	if got := foldRoles(t, out); len(got) != 3 {
		t.Fatalf("roles=%v want 3", got)
	}
	var obj map[string]any
	json.Unmarshal(out, &obj)
	msgs := obj["messages"].([]any)
	if c, _ := msgs[0].(map[string]any)["content"].(string); c != "SYS\n\n第一问" {
		t.Errorf("first user=%q", c)
	}
	if c, _ := msgs[1].(map[string]any)["content"].(string); c != "第一答" {
		t.Errorf("assistant changed: %q", c)
	}
	if c, _ := msgs[2].(map[string]any)["content"].(string); c != "第二问" {
		t.Errorf("later user touched: %q", c)
	}
}

// TestFoldSystemMessagesNoUser F4：没有 user 消息时在最前插入一条 user 承载系统提示词。
func TestFoldSystemMessagesNoUser(t *testing.T) {
	in := []byte(`{"messages":[
		{"role":"system","content":"SYS"},
		{"role":"assistant","content":"现有回答"}
	]}`)
	out := FoldSystemMessages(in)
	got := foldRoles(t, out)
	if len(got) != 2 || got[0] != "user" || got[1] != "assistant" {
		t.Fatalf("roles=%v want [user assistant]", got)
	}
	if c := foldFirstUser(t, out); c != "SYS" {
		t.Errorf("inserted user content=%q", c)
	}
}

// TestFoldSystemMessagesNoop 无 system/developer 时返回原 body（不重新序列化，
// 保证零副作用——字节级相同）。
func TestFoldSystemMessagesNoop(t *testing.T) {
	in := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	out := FoldSystemMessages(in)
	if string(out) != string(in) {
		t.Errorf("should return body unchanged, got %s", out)
	}
}

// TestFoldSystemMessagesEmptySystemKept F5：system 内容全为空时整条链路退化为
// no-op（原样返回）——没有可折叠的文本就不改动请求，空 system 消息留给上游
// 按既有语义忽略，避免"为了折叠而引入空行/新消息"。
func TestFoldSystemMessagesEmptySystemKept(t *testing.T) {
	in := []byte(`{"messages":[
		{"role":"system","content":""},
		{"role":"system","content":"   "},
		{"role":"user","content":"问题"}
	]}`)
	out := FoldSystemMessages(in)
	if string(out) != string(in) {
		t.Errorf("all-empty system must be a no-op, got %s", out)
	}
	if c := foldFirstUser(t, out); c != "问题" {
		t.Errorf("user content=%q want 问题（不引入前导空行）", c)
	}
}

// TestFoldSystemMessagesMultimodalContent F6：多模态 content（[]any）在头部插入
// 一个 text 分区，既有图片分区原位保留、类型不变。
func TestFoldSystemMessagesMultimodalContent(t *testing.T) {
	in := []byte(`{"messages":[
		{"role":"system","content":"SYS"},
		{"role":"user","content":[
			{"type":"text","text":"看这张图"},
			{"type":"image_url","image_url":{"url":"https://x/y.png"}}
		]}
	]}`)
	out := FoldSystemMessages(in)
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("unmarshal: %v %s", err, out)
	}
	msgs := obj["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("msgs=%d want 1: %s", len(msgs), out)
	}
	parts, ok := msgs[0].(map[string]any)["content"].([]any)
	if !ok {
		t.Fatalf("content not []any: %s", out)
	}
	if len(parts) != 3 {
		t.Fatalf("parts=%d want 3 (folded text + original text + image): %s", len(parts), out)
	}
	head := parts[0].(map[string]any)
	if head["type"] != "text" || head["text"] != "SYS" {
		t.Errorf("folded part=%v", head)
	}
	second := parts[1].(map[string]any)
	if second["type"] != "text" || second["text"] != "看这张图" {
		t.Errorf("original text part changed: %v", second)
	}
	img := parts[2].(map[string]any)
	if img["type"] != "image_url" {
		t.Errorf("image part changed: %v", img)
	}
}

// TestFoldSystemMessagesMultimodalSystem F7：system 消息本身是多模态形态时，
// 只提取其文本分区参与折叠。
func TestFoldSystemMessagesMultimodalSystem(t *testing.T) {
	in := []byte(`{"messages":[
		{"role":"system","content":[{"type":"text","text":"A"},{"type":"text","text":"B"}]},
		{"role":"user","content":"问题"}
	]}`)
	out := FoldSystemMessages(in)
	if c := foldFirstUser(t, out); c != "A\n\nB\n\n问题" {
		t.Errorf("content=%q want A\\n\\nB\\n\\n问题", c)
	}
}

// TestFoldSystemMessagesBadJSON 坏 body / 空 body / 无 messages → 原样返回，绝不失败。
func TestFoldSystemMessagesBadJSON(t *testing.T) {
	for _, in := range []string{"", "not json", `{"model":"m"}`, `{"messages":"oops"}`} {
		out := FoldSystemMessages([]byte(in))
		if string(out) != in {
			t.Errorf("input %q should return unchanged, got %q", in, out)
		}
	}
}

// TestPrepareBodyFoldOffByDefault 默认（未开启开关）时经生产管线不折叠：
// system 消息仍以 system 出站（PrepareBodyOptWithEffortsAndDefault 不涉及折叠，
// 折叠挂在 Client.FoldSystemMessages 上，见 client.go ChatStreamContext）。
func TestPrepareBodyFoldOffByDefault(t *testing.T) {
	in := []byte(`{"messages":[{"role":"system","content":"SYS"},{"role":"user","content":"u"}]}`)
	out := PrepareBodyOptWithEffortsAndDefault(in, true, nil, nil)
	got := foldRoles(t, out)
	if len(got) != 2 || got[0] != "system" {
		t.Fatalf("pipeline must not fold: roles=%v body=%s", got, out)
	}
}
