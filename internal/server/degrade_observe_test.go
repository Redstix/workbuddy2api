package server

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

// TestHealthzCarriesDegradedBothRealms 核心要求：/healthz 恒输出 degraded 的
// cn 与 global 两个键，即使都为 false。缺键与 false 在告警规则里分不清——
// `degraded.global == true` 在键缺失时恒不触发，会静默漏报。
func TestHealthzCarriesDegradedBothRealms(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: upstream.New()})

	read := func() map[string]any {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
		var resp map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("healthz not json: %v body=%s", err, rec.Body)
		}
		d, ok := resp["degraded"].(map[string]any)
		if !ok {
			t.Fatalf("healthz missing degraded object: %v", resp)
		}
		return d
	}

	// 未降级：两个键都必须存在且为 false。
	d := read()
	for _, realm := range []string{"cn", "global"} {
		v, ok := d[realm]
		if !ok {
			t.Fatalf("degraded missing key %q (gap: alert rules cannot distinguish missing from false): %v", realm, d)
		}
		if v != false {
			t.Errorf("degraded[%q]=%v want false", realm, v)
		}
	}

	// 只触发 cn：global 必须仍为 false（realm 隔离在对外接口上同样成立）。
	h.degrade.Trigger("cn")
	d = read()
	if d["cn"] != true {
		t.Errorf("degraded[cn]=%v want true after Trigger(cn)", d["cn"])
	}
	if d["global"] != false {
		t.Errorf("degraded[global]=%v want false (cn incident must not leak)", d["global"])
	}
}

// TestPromMetricsDegradeActiveBothRealms /metrics 恒输出 cn/global 两条
// wb2api_degrade_active。序列缺失会让 `== 1` 的告警恒不触发，所以哪怕没有降级
// 也必须出现，值为 0。
func TestPromMetricsDegradeActiveBothRealms(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	// MetricsEnabled: /metrics 路由默认不注册（MetricsEnabled=false → 404），
	// 这里要测的是渲染内容，必须把路由打开。
	h := NewHandler(Config{Pool: p, Upstream: upstream.New(), MetricsEnabled: true})

	scrape := func() string {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
		return rec.Body.String()
	}

	out := scrape()
	if !strings.Contains(out, "# TYPE wb2api_degrade_active gauge") {
		t.Fatalf("missing wb2api_degrade_active family:\n%s", out)
	}
	for _, realm := range []string{"cn", "global"} {
		want := `wb2api_degrade_active{realm="` + realm + `"} 0`
		if !strings.Contains(out, want) {
			t.Errorf("missing sample %q (a missing series makes `wb2api_degrade_active == 1` silently never fire):\n%s", want, out)
		}
	}

	// 只触发 global → cn 必须是 0、global 是 1。
	h.degrade.Trigger("global")
	out = scrape()
	if !strings.Contains(out, `wb2api_degrade_active{realm="cn"} 0`) {
		t.Errorf("cn should stay 0:\n%s", out)
	}
	if !strings.Contains(out, `wb2api_degrade_active{realm="global"} 1`) {
		t.Errorf("global should be 1 after Trigger(global):\n%s", out)
	}
}

// TestDegradedBranchWarnsThrottled 降级分支必须真的打日志，且每分钟最多一条。
// 降级期原本零日志是这个问题藏两个月的主因；这里把「真的会打」也锁住——
// 只测 noteWarn 的返回值不够，调用点漏了就白测。
func TestDegradedBranchWarnsThrottled(t *testing.T) {
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
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
	h.degrade.Trigger("cn") // 预置降级期

	var buf bytes.Buffer
	prevOut := log.Writer()
	prevFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() {
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	}()

	for i := 0; i < 3; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
			strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
		if rec.Code != 200 {
			t.Fatalf("req #%d code=%d body=%s", i+1, rec.Code, rec.Body)
		}
	}

	got := strings.Count(buf.String(), "serving in DEGRADED prompt mode realm=cn")
	if got != 1 {
		t.Errorf("want exactly 1 degraded WARN in the first minute, got %d:\n%s", got, buf.String())
	}
}
