// proxy_test.go 账号级出站代理在 auth 层的两条硬约束：
//
//  1. **落盘往返不丢**（最重要）：proxy_addr 必须能被 Parse 读出来、被 SaveAtomic 写回去。
//     SaveAtomic 是「固定字段集整体重写」，token 刷新走的就是这条高频路径——漏写
//     等于每次续期都把代理地址从磁盘上抹掉，而账号不会报错，只是静默退回直连。
//  2. **realm 闸门**：只有 global 账号才真正启用代理；cn 账号即使配了 proxy_addr
//     也必须直连（把国内账号的出口挪到海外节点是给风控送特征）。
package auth

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestParseProxyNested 嵌套形（插件 OAuth 输出形态）能读出 proxy_addr。
//
// 三个字段都放在顶层，与 groups / device_token 同位置——手写时不用嵌进 auth 段。
func TestParseProxyNested(t *testing.T) {
	raw := []byte(`{
		"auth": {"accessToken":"at","refreshToken":"rt","expiresAt":1,"domain":"www.workbuddy.ai","realm":"global"},
		"account": {"uid":"g1","enterpriseId":"e1","nickname":"n1"},
		"proxy_addr": "http://127.0.0.1:17891",
		"proxy_node": "hk-01",
		"proxy_region": "HK"
	}`)
	a, err := Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if a.ProxyAddr != "http://127.0.0.1:17891" {
		t.Errorf("ProxyAddr=%q", a.ProxyAddr)
	}
	if a.ProxyNode != "hk-01" || a.ProxyRegion != "HK" {
		t.Errorf("ProxyNode/Region=%q/%q", a.ProxyNode, a.ProxyRegion)
	}
}

// TestParseProxyFlat 扁平形（手写形态）同样能读出三个字段。
func TestParseProxyFlat(t *testing.T) {
	raw := []byte(`{"accessToken":"at","refreshToken":"rt","expiresAt":1,
		"domain":"www.workbuddy.ai","realm":"global","uid":"g1",
		"proxy_addr":" http://127.0.0.1:17892 ","proxy_node":"jp-02","proxy_region":"JP"}`)
	a, err := Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// 首尾空白必须被裁掉：地址来自人手填写/脚本拼装，带空格时 url.Parse 会失败，
	// 而失败点离配置处很远（表现为请求全挂），所以在 Parse 入口就规范化。
	if a.ProxyAddr != "http://127.0.0.1:17892" {
		t.Errorf("ProxyAddr=%q (leading/trailing space must be trimmed)", a.ProxyAddr)
	}
	if a.ProxyNode != "jp-02" || a.ProxyRegion != "JP" {
		t.Errorf("ProxyNode/Region=%q/%q", a.ProxyNode, a.ProxyRegion)
	}
}

// TestSaveAtomicRoundtripProxy 落盘往返：SaveAtomic 之后再 Parse，代理字段必须还在。
//
// 这是本组测试里最重要的一条。SaveAtomic 用 map[string]any 固定字段集整体重写文件，
// 任何没被显式写回的键都会从磁盘消失；而它被 token 刷新（RefreshToken → SaveAtomic）
// 高频调用。漏写 proxy_addr 的后果是「某次续期后账号悄悄开始直连」——不报错、不告警，
// 等上游 403 收紧才发现，且已无法回溯是哪一次刷新吃掉的。
func TestSaveAtomicRoundtripProxy(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "workbuddy-g1.json")
	a := &Auth{
		AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1753600000,
		UID: "g1", Nickname: "n1", FilePath: fp,
		Domain:      "www.workbuddy.ai",
		ProxyAddr:   "http://127.0.0.1:17891",
		ProxyNode:   "hk-01",
		ProxyRegion: "HK",
	}
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("save: %v", err)
	}
	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	b, err := Parse(raw)
	if err != nil {
		t.Fatalf("reparse: %v", err)
	}
	if b.ProxyAddr != a.ProxyAddr {
		t.Fatalf("proxy_addr lost across SaveAtomic: got %q want %q (token refresh would silently drop it)", b.ProxyAddr, a.ProxyAddr)
	}
	if b.ProxyNode != a.ProxyNode || b.ProxyRegion != a.ProxyRegion {
		t.Errorf("proxy_node/region lost: %q/%q", b.ProxyNode, b.ProxyRegion)
	}
}

// TestSaveAtomicOmitsEmptyProxy 未配代理的账号不该被写入空的 proxy_addr 键。
//
// 与 device_token / groups 同一策略：不给旧文件引入无意义空键（插件读取忽略未知键，
// 但空串键会让「配置过代理」这件事在文件里看起来存在，排查时误导人）。
func TestSaveAtomicOmitsEmptyProxy(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "workbuddy-c1.json")
	a := &Auth{AccessToken: "at", UID: "c1", FilePath: fp}
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("save: %v", err)
	}
	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	for _, key := range []string{"proxy_addr", "proxy_node", "proxy_region"} {
		if containsKey(raw, key) {
			t.Errorf("%q must not be written when empty: %s", key, raw)
		}
	}
}

// TestOutboundProxyAddrRealmGate realm 闸门：只有 global 账号才启用代理。
//
// 这是整个特性的唯一判定点（upstream 侧不再各自判断），所以四种组合都要锁住。
func TestOutboundProxyAddrRealmGate(t *testing.T) {
	SetGlobalEnabled(true)
	t.Cleanup(func() { SetGlobalEnabled(true) })

	cases := []struct {
		name string
		a    *Auth
		want string
	}{
		{
			name: "global 有代理 → 用代理",
			a:    &Auth{Domain: "www.workbuddy.ai", realm: "global", ProxyAddr: "http://127.0.0.1:17891"},
			want: "http://127.0.0.1:17891",
		},
		{
			name: "global 无代理 → 直连",
			a:    &Auth{Domain: "www.workbuddy.ai", realm: "global"},
			want: "",
		},
		{
			name: "cn 配了代理 → 仍直连",
			a:    &Auth{Domain: "www.codebuddy.cn", ProxyAddr: "http://127.0.0.1:17891"},
			want: "",
		},
		{
			name: "cn 无代理 → 直连",
			a:    &Auth{Domain: ""},
			want: "",
		},
		{
			name: "domain 后缀判 global（无显式 realm）→ 用代理",
			a:    &Auth{Domain: "api.workbuddy.ai", ProxyAddr: "http://127.0.0.1:17891"},
			want: "http://127.0.0.1:17891",
		},
	}
	for _, c := range cases {
		if got := c.a.OutboundProxyAddr(); got != c.want {
			t.Errorf("%s: OutboundProxyAddr()=%q want %q", c.name, got, c.want)
		}
	}
}

// TestOutboundProxyAddrRespectsGlobalEscapeHatch 逃生门（global.enabled=false）下
// 即使账号写着 realm=global 也不启用代理。
//
// 逃生门语义是「这台机器锁死纯 CN」：不仅路由要回 CN base，出口身份也必须回直连。
// 只锁路由不锁代理会留下矛盾状态——请求打 CN 上游却从海外节点出去。
func TestOutboundProxyAddrRespectsGlobalEscapeHatch(t *testing.T) {
	SetGlobalEnabled(false)
	t.Cleanup(func() { SetGlobalEnabled(true) })

	a := &Auth{Domain: "www.workbuddy.ai", realm: "global", ProxyAddr: "http://127.0.0.1:17891"}
	if got := a.OutboundProxyAddr(); got != "" {
		t.Errorf("escape hatch: OutboundProxyAddr()=%q want empty", got)
	}
	// ProxyAddrValue 不做闸门，必须如实返回配置值——它是「文件里写了什么」的观测口径。
	if got := a.ProxyAddrValue(); got != "http://127.0.0.1:17891" {
		t.Errorf("ProxyAddrValue()=%q want raw configured value", got)
	}
}

// TestProxyAddrNilSafe 方法必须 nil 安全：池里可能有过被移除账号的 *Auth 残留指针。
func TestProxyAddrNilSafe(t *testing.T) {
	var a *Auth
	if got := a.ProxyAddrValue(); got != "" {
		t.Errorf("nil.ProxyAddrValue()=%q", got)
	}
	if got := a.OutboundProxyAddr(); got != "" {
		t.Errorf("nil.OutboundProxyAddr()=%q", got)
	}
}

// containsKey 粗判 JSON 文本里是否出现某个键（只需判断"有没有这个键"，不必解析）。
func containsKey(raw []byte, key string) bool {
	return len(raw) > 0 && strings.Contains(string(raw), `"`+key+`"`)
}
