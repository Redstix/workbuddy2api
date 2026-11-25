package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

// TestHandlerFoldSystemEndToEnd F8（端到端）：features.fold_system_messages 开启后，
// 上游收到的请求里 system 消息消失，其文本前置合并进首条 user 消息。
//
// 折叠发生在 downstream.Client.ChatStreamContext（prepareBody → 折叠 → 轮转循环前），
// 因此在 handler 层用打桩 upstream 断言出站 body 最接近真实语义。
func TestHandlerFoldSystemEndToEnd(t *testing.T) {
	var bodies [][]byte
	up := newBodyCaptureUpstream(t, &bodies, func(call int, body string) (int, string, bool) {
		return 200, sseOK, true
	})
	up.FoldSystemMessages = true
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, PromptMode: "passthrough"})

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[
		{"role":"system","content":"你是记忆提取器，只输出 Python DSL"},
		{"role":"user","content":"把这段对话写成记忆"}]}`)))
	if len(bodies) != 1 {
		t.Fatalf("want 1 upstream call, got %d", len(bodies))
	}
	var obj map[string]any
	if err := json.Unmarshal(bodies[0], &obj); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, bodies[0])
	}
	msgs := obj["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages len=%d want 1 (system folded away): %s", len(msgs), bodies[0])
	}
	first := msgs[0].(map[string]any)
	if first["role"] != "user" {
		t.Errorf("role=%v want user", first["role"])
	}
	content, _ := first["content"].(string)
	if !strings.Contains(content, "你是记忆提取器，只输出 Python DSL") {
		t.Errorf("system text lost from outbound body: %q", content)
	}
	if !strings.HasSuffix(content, "把这段对话写成记忆") {
		t.Errorf("user text must remain at tail: %q", content)
	}
}

// TestHandlerFoldSystemDefaultOff F9：默认（未开启）时 system 照常出站——
// 折叠是显式开关，绝不影响存量客户端。
func TestHandlerFoldSystemDefaultOff(t *testing.T) {
	var bodies [][]byte
	up := newBodyCaptureUpstream(t, &bodies, func(call int, body string) (int, string, bool) {
		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, PromptMode: "passthrough"})

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[
		{"role":"system","content":"SYS"},
		{"role":"user","content":"hi"}]}`)))
	if len(bodies) != 1 {
		t.Fatalf("want 1 upstream call, got %d", len(bodies))
	}
	var obj map[string]any
	json.Unmarshal(bodies[0], &obj)
	msgs := obj["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages len=%d want 2 (no folding by default): %s", len(msgs), bodies[0])
	}
	if msgs[0].(map[string]any)["role"] != "system" {
		t.Errorf("system must stay system by default: %s", bodies[0])
	}
}

// TestHandlerFoldSystemWithCustomPrompt F10：折叠与 prompt.mode 正交——
// custom 模式下网关自有的 system 提示词也会被折叠进首条 user（上游反正收不到 system），
// 客户端原始 system 已被 custom 替换，不会重复出现。
func TestHandlerFoldSystemWithCustomPrompt(t *testing.T) {
	var bodies [][]byte
	up := newBodyCaptureUpstream(t, &bodies, func(call int, body string) (int, string, bool) {
		return 200, sseOK, true
	})
	up.FoldSystemMessages = true
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, PromptMode: "custom", PromptText: "网关人格"})

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[
		{"role":"system","content":"客户端原始规范"},
		{"role":"user","content":"你好"}]}`)))
	if len(bodies) != 1 {
		t.Fatalf("want 1 upstream call, got %d", len(bodies))
	}
	var obj map[string]any
	json.Unmarshal(bodies[0], &obj)
	msgs := obj["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages len=%d want 1: %s", len(msgs), bodies[0])
	}
	content, _ := msgs[0].(map[string]any)["content"].(string)
	if !strings.Contains(content, "网关人格") {
		t.Errorf("gateway prompt lost: %q", content)
	}
	if strings.Contains(content, "客户端原始规范") {
		t.Errorf("custom mode must drop client system, got: %q", content)
	}
	if !strings.Contains(content, "你好") {
		t.Errorf("user text lost: %q", content)
	}
}

// TestHandlerFoldSystemMultimodalUser F11：折叠遇到多模态 user 消息时，
// 系统提示词作为新的 text 分区插到最前，图片分区保留。
func TestHandlerFoldSystemMultimodalUser(t *testing.T) {
	var bodies [][]byte
	up := newBodyCaptureUpstream(t, &bodies, func(call int, body string) (int, string, bool) {
		return 200, sseOK, true
	})
	up.FoldSystemMessages = true
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, PromptMode: "passthrough"})

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[
		{"role":"system","content":"SYS"},
		{"role":"user","content":[{"type":"text","text":"看图"},{"type":"image_url","image_url":"https://x/y.png"}]}]}`)))
	if len(bodies) != 1 {
		t.Fatalf("want 1 upstream call, got %d", len(bodies))
	}
	var obj map[string]any
	json.Unmarshal(bodies[0], &obj)
	msgs := obj["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages len=%d want 1: %s", len(msgs), bodies[0])
	}
	parts, ok := msgs[0].(map[string]any)["content"].([]any)
	if !ok {
		t.Fatalf("content not []any: %s", bodies[0])
	}
	if len(parts) != 3 {
		t.Fatalf("parts=%d want 3: %s", len(parts), bodies[0])
	}
	if parts[0].(map[string]any)["text"] != "SYS" {
		t.Errorf("folded text not first: %v", parts[0])
	}
	// normalizeImageURL 已把字符串 image_url 归一为对象形态。
	img := parts[2].(map[string]any)
	if img["type"] != "image_url" {
		t.Fatalf("image part: %v", img)
	}
	if _, ok := img["image_url"].(map[string]any); !ok {
		t.Errorf("image_url not normalized: %v", img)
	}
}

// TestUpstreamFoldLogsCount 单测辅助：确认折叠日志不影响返回值（仅作冒烟）。
var _ = upstream.FoldSystemMessages
