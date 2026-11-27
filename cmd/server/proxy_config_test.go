package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestProxyOnFailureConfigDefault 默认 block：绝不静默改出口。
//
// 默认值本身就是安全策略的一部分——如果这里默成 direct，所有没配 proxy 段的
// 部署都会在代理挂掉时悄悄用机房 IP 打上游，而配置文件里看不出任何异常。
func TestProxyOnFailureConfigDefault(t *testing.T) {
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.Proxy.OnFailure != "block" {
		t.Errorf("proxy.on_failure default=%q want \"block\"", c.Proxy.OnFailure)
	}
}

// TestProxyOnFailureConfigJSON 显式 JSON 配置。
func TestProxyOnFailureConfigJSON(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"proxy":{"on_failure":"direct"}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Proxy.OnFailure != "direct" {
		t.Errorf("proxy.on_failure=%q want \"direct\"", c.Proxy.OnFailure)
	}
}

// TestProxyOnFailureConfigEnv WB2A_PROXY_ON_FAILURE 环境变量入口。
func TestProxyOnFailureConfigEnv(t *testing.T) {
	t.Setenv("WB2A_PROXY_ON_FAILURE", "direct")
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.Proxy.OnFailure != "direct" {
		t.Errorf("WB2A_PROXY_ON_FAILURE=direct not applied, got %q", c.Proxy.OnFailure)
	}
}

// TestProxyOnFailureNormalized 大小写与空白容错：配置是人写的，别让 "Direct " 变成
// 非法值，也别让它悄悄落到默认 block（那样运维以为自己开了调试模式，实际没开）。
func TestProxyOnFailureNormalized(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"proxy":{"on_failure":"  DiReCt  "}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatalf("case/space should be tolerated: %v", err)
	}
	if c.Proxy.OnFailure != "direct" {
		t.Errorf("proxy.on_failure=%q want \"direct\" (normalized)", c.Proxy.OnFailure)
	}
}

// TestProxyOnFailureInvalidFailsFast 非法值必须让启动失败。
//
// 不能静默回落到 block 了事：运维写 "blocked"/"Block" 的本意可能是想关掉降级，
// 静默按默认跑会让他以为配置生效了；反过来若静默按 direct 跑则直接把风控暴露
// 出去。这里的选择是「配置写错就当场拒绝启动」，让错误在部署时就暴露。
func TestProxyOnFailureInvalidFailsFast(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"proxy":{"on_failure":"blocked"}}`), 0o600)
	_, err := Load(fp)
	if err == nil {
		t.Fatal("want error for invalid proxy.on_failure, got nil")
	}
	if !strings.Contains(err.Error(), "proxy.on_failure") {
		t.Errorf("error should name the offending key, got: %v", err)
	}
}
