package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/prompt"
	"workbuddy2api/internal/upstream"
)

// TestRotateBudgetReservedForDegradeRetry maxAttempts++ 的唯一价值：
// 撞 400 content_blocked 的那一轮如果是**最后一轮**，没有额外预算的话降级重试
// 就没名额了，400 会直接透传给客户端——而重试本来是能过的。
//
// 场景：MaxRotate=3，三号池
//
//	第 1 轮 → u1 402 hard_credit（换号）
//	第 2 轮 → u2 402 hard_credit（换号）
//	第 3 轮 → u3 400 content_blocked（最后一轮！触发降级重试）
//	第 4 轮 → 有 maxAttempts++ 才有这一轮：Degraded 提示词 → 200
func TestRotateBudgetReservedForDegradeRetry(t *testing.T) {
	var bodies [][]byte
	calls := 0
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(r.Body)
			bodies = append(bodies, raw)
			calls++
			switch calls {
			case 1, 2:
				return &http.Response{
					StatusCode: 402,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(`{"code":1,"msg":"credits exhausted"}`)),
				}, nil
			case 3:
				return &http.Response{
					StatusCode: 400,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(`{"code":11-128,"msg":"blocked by security policy"}`)),
				}, nil
			default:
				return &http.Response{
					StatusCode: 200,
					Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
					Body:       io.NopCloser(strings.NewReader(sseOK)),
				}, nil
			}
		})},
		ChatBaseCN: "https://fake.example",
	}
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u3", AccessToken: "at3", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up, PromptMode: "passthrough"})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"system","content":"原始指纹"},{"role":"user","content":"hi"}]}`)))

	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s (want 200: the content_blocked hit the LAST rotation round, "+
			"the reserved attempt must still retry with the degraded prompt)", rec.Code, rec.Body)
	}
	if calls != 4 {
		t.Fatalf("want 4 upstream calls (2 hard_credit + 1 blocked + 1 degraded retry), got %d", calls)
	}
	if !strings.Contains(string(bodies[3]), prompt.Degraded) {
		t.Errorf("retry body should carry the Degraded prompt: %s", bodies[3])
	}
	if strings.Contains(string(bodies[3]), "原始指纹") {
		t.Errorf("retry body should not carry the original system: %s", bodies[3])
	}
}

// TestRotateBudgetUnchangedForNonDegradePaths 反证：maxAttempts++ 只作用于降级路径。
// 同规模的三号池、三轮全 hard_credit（非降级错误）→ 仍然只轮转 MaxRotate=3 次，
// 不会因为「多留一轮」的改动而多烧一个号的机会。
func TestRotateBudgetUnchangedForNonDegradePaths(t *testing.T) {
	calls := 0
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			calls++
			return &http.Response{
				StatusCode: 402,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"code":1,"msg":"credits exhausted"}`)),
			}, nil
		})},
		ChatBaseCN: "https://fake.example",
	}
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u3", AccessToken: "at3", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up, PromptMode: "passthrough"})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))

	if rec.Code == 200 {
		t.Fatalf("all upstream calls hard_credit should not end 200: body=%s", rec.Body)
	}
	if calls != 3 {
		t.Errorf("non-degrade path must keep MaxRotate=3 attempts, got %d", calls)
	}
}

// TestDegradeRetryDoesNotRetrigger 降级期内第二个请求：Active(realm) 已为 true，
// body 在轮转前就被换成 Degraded，因此走的是「首轮即成功」而不是再次 Trigger。
// 断言 backoff 不被第二个请求再次翻倍（阶梯只由「窗口到期后仍被拦」驱动）。
func TestDegradeRetryDoesNotRetrigger(t *testing.T) {
	calls := 0
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			calls++
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(sseOK)),
			}, nil
		})},
		ChatBaseCN: "https://fake.example",
	}
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, PromptMode: "passthrough"})

	// 预置降级期（模拟此前已撞过一次 content_blocked）。
	h.degrade.Trigger("cn")
	backoffBefore, _ := gateState(h.degrade.forRealm("cn"))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"system","content":"x"},{"role":"user","content":"hi"}]}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if calls != 1 {
		t.Errorf("degraded window should go straight to upstream once, got %d calls", calls)
	}
	backoffAfter, _ := gateState(h.degrade.forRealm("cn"))
	if backoffAfter != backoffBefore {
		t.Errorf("second request in degraded window must not double backoff: before=%v after=%v", backoffBefore, backoffAfter)
	}
}
