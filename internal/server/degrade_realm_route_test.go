package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

// P1-1：handler「把当前请求的 realm 传进降级门」这一步的覆盖。
//
// 背景：realm 隔离在 gate 层已有测试（TestDegradeSetRealmIsolation），但那只是直接
// 调 degradeSet 的方法。**handler 把 realm 传进 Active/Trigger 这一步此前零覆盖**：
// 其余降级测试全用 cn 账号、靠 Trigger("cn") 手工预置，从不走 handler 自己的触发
// 分支；全仓唯一发 global: chat 的 global_e2e_test.go 带 //go:build global_e2e，
// 默认不编译。
//
// 变异反证（本测试就是为抓它而写）：把 handler 里五处降级调用点的 realm 硬编码成
// "cn"，补这条测试之前 go test ./... 仍然 20/20 全绿——即 realm 传错了没人发现。
//
// 走真实 ServeHTTP：发一条 global: 请求，fake 上游一律 400 content_blocked，
// 断言触发的是 global 的门、没碰 cn 的门。
func TestDegradeTriggerUsesRequestRealm(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })

	blockedUp := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: 400,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"code":"11-128","msg":"blocked by security policy"}`)),
			}, nil
		})},
		ChatBaseCN:        "https://fake.cn",
		BillingBaseCN:     "https://fake.cn",
		ChatBaseGlobal:    "https://fake.global",
		BillingBaseGlobal: "https://fake.global",
		GlobalEnabled:     true,
	}
	accounts := func() []*auth.Auth {
		return []*auth.Auth{
			{UID: "cn1", AccessToken: "at_cn", Domain: "www.codebuddy.cn", ExpiresAt: 9999999999},
			{UID: "g1", AccessToken: "at_gl", Domain: "www.workbuddy.ai", ExpiresAt: 9999999999},
		}
	}

	// global: 请求 → 只应触发 global 的门。
	h := NewHandler(Config{Pool: testPoolWith(accounts()...), Upstream: blockedUp, GlobalEnabled: true, PromptMode: "passthrough"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"global:gpt-5.4","stream":true,"messages":[{"role":"system","content":"指纹"},{"role":"user","content":"hi"}]}`)))
	if h.degrade.Active("cn") {
		t.Error("global: request must NOT trigger the cn degrade gate (cross-realm contamination is exactly the bug this fix removes)")
	}
	if !h.degrade.Active("global") {
		t.Fatalf("global: request must trigger the global degrade gate — handler passed the wrong realm to Trigger/Active (code=%d body=%s)", rec.Code, rec.Body)
	}

	// 反向：cn 请求只碰 cn 的门，且不得清掉已处于降级期的 global 门。
	h2 := NewHandler(Config{Pool: testPoolWith(accounts()...), Upstream: blockedUp, GlobalEnabled: true, PromptMode: "passthrough"})
	h2.degrade.Trigger("global")
	rec = httptest.NewRecorder()
	h2.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"system","content":"指纹"},{"role":"user","content":"hi"}]}`)))
	if !h2.degrade.Active("global") {
		t.Error("a cn request must not reset the global gate")
	}
	if !h2.degrade.Active("cn") {
		t.Errorf("cn request should trigger the cn gate (code=%d body=%s)", rec.Code, rec.Body)
	}
}

// P1-2：恢复路径的覆盖 —— 成功分支必须真的调 ResetBackoff。
//
// 背景：TestDegradeGateResetBackoff 只测 gate 自己归零（直接调方法），
// TestDegradeRetryDoesNotRetrigger 走的是 degradedApplied=true 分支。
// **没有任何测试走「成功 + !degradedApplied → ResetBackoff」这条路径**，
// 所以把 handler 里那三行 ResetBackoff 整段删掉，补这条测试之前 go test ./... 仍全绿。
//
// 本测试：先制造一次降级（backoff=30m），把窗口拨到过期，再让一个原始提示词请求
// 成功（= 上游已放开），断言阶梯归 0、until 清空。
func TestDegradeResetBackoffOnSuccessfulProbe(t *testing.T) {
	okUp := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(sseOK)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}), Upstream: okUp, PromptMode: "passthrough"})

	h.degrade.Trigger("cn")
	gate := h.degrade.forRealm("cn")
	if backoff, _ := gateState(gate); backoff != degradeBackoffInitial {
		t.Fatalf("precondition: backoff=%v want %v", backoff, degradeBackoffInitial)
	}
	if !h.degrade.Active("cn") {
		t.Fatal("precondition: gate should be active")
	}
	// 把窗口拨到已过期：等价于「降级窗口到期后用真实提示词试探」。
	gate.mu.Lock()
	gate.until = gate.clock().Add(-time.Second)
	gate.mu.Unlock()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"system","content":"原始指纹"},{"role":"user","content":"hi"}]}`)))
	if rec.Code != 200 {
		t.Fatalf("probe request code=%d body=%s", rec.Code, rec.Body)
	}

	backoff, until := gateState(gate)
	if backoff != 0 {
		t.Errorf("successful probe with the ORIGINAL prompt must reset the ladder to 0 (next incident starts at %v), got %v", degradeBackoffInitial, backoff)
	}
	if !until.IsZero() {
		t.Errorf("successful probe should clear until, got %v", until)
	}
	if h.degrade.Active("cn") {
		t.Error("gate should no longer be active after a successful probe")
	}
}

// P1-2 的反向断言：靠 Degraded 中性提示词换来的成功**不得**归零阶梯。
//
// 若这里归零，降级期内的每一次成功都会把阶梯清零，退避永远升不上去——
// 事故持续时每个窗口都还是 30m，退避机制等于失效。
func TestDegradeSuccessWithDegradedPromptKeepsLadder(t *testing.T) {
	okUp := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(sseOK)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}), Upstream: okUp, PromptMode: "passthrough"})

	h.degrade.Trigger("cn")
	gate := h.degrade.forRealm("cn")
	backoffBefore, untilBefore := gateState(gate)

	// 降级期仍有效 → 请求走 degradedApplied=true 分支（Degraded 提示词）。
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"system","content":"原始指纹"},{"role":"user","content":"hi"}]}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	backoffAfter, untilAfter := gateState(gate)
	if backoffAfter != backoffBefore {
		t.Errorf("success via the DEGRADED prompt must not reset the ladder (upstream still blocks the original system): before=%v after=%v", backoffBefore, backoffAfter)
	}
	if !untilAfter.Equal(untilBefore) {
		t.Errorf("success via the DEGRADED prompt must not clear until: before=%v after=%v", untilBefore, untilAfter)
	}
	if !h.degrade.Active("cn") {
		t.Error("gate must remain active while the incident persists")
	}
}

// P3：ResetBackoff 必须一并清 lastWarn。
//
// 限流是「同一场事故不刷屏」的手段，不是跨事故的配额。不清 lastWarn 的话，
// 新事故若在前一场事故最后一次 WARN 后的 60 秒内发生，新事故的第一条 WARN
// 会被上一场的限流吃掉——新事故头一分钟反而静默，正是这次要消灭的情形。
func TestResetBackoffClearsWarnThrottle(t *testing.T) {
	g, _ := newGateWithClock()
	if !g.noteWarn() {
		t.Fatal("first warn should be allowed")
	}
	if g.noteWarn() {
		t.Fatal("precondition: second warn within a minute should be throttled")
	}
	g.ResetBackoff()
	if !g.noteWarn() {
		t.Error("after ResetBackoff the next incident's first WARN must not be throttled by the previous incident")
	}
}
