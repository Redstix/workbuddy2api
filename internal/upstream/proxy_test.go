// proxy_test.go 账号级出站代理的回归测试。
//
// 锁住四件事（顺序即重要性）：
//  1. global + proxy_addr → 请求真的经过那个代理（假代理必须收到请求）；
//  2. cn + proxy_addr → 仍然直连（闸门在 auth.OutboundProxyAddr 里唯一实现）；
//  3. global 无 proxy_addr → 直连（还没分配节点不该报错）；
//  4. 代理不可达 → 返回错误，**绝不**静默退回直连（这是整套设计的安全底线）。
//
// 另有 auth 包的 ProxyAddr 落盘往返（见 internal/auth/proxy_test.go）。
package upstream

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"workbuddy2api/internal/auth"
)

// globalAcct 构造一个 global realm 账号（domain 后缀 .workbuddy.ai 即判 global）。
// realm 字段是 auth 包私有的，测试只能走 domain 这条公开路径。
func proxyGlobalAcct(proxyAddr string) *auth.Auth {
	return &auth.Auth{
		AccessToken: "at",
		UID:         "g1",
		Domain:      "www.workbuddy.ai",
		ProxyAddr:   proxyAddr,
	}
}

// cnAcct 构造一个 CN 账号（空 domain → cn，老凭证形态）。
func proxyCNAcct(proxyAddr string) *auth.Auth {
	return &auth.Auth{
		AccessToken: "at",
		UID:         "cn1",
		Domain:      "www.codebuddy.cn",
		ProxyAddr:   proxyAddr,
	}
}

// directCounter 一个「直连侧」client：记录被调用次数，返回固定 JSON 信封。
// 用它代替真实网络，断言「有没有走直连」这件事本身。
func proxyDirectCounter(hits *int32, body string) *http.Client {
	return &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
		atomic.AddInt32(hits, 1)
		return jsonResp(200, body), nil
	})}
}

// TestGlobalWithProxyGoesThroughProxy global + proxy_addr → 请求必须到达代理。
//
// 假代理是一个 httptest 服务器：Go 的 Transport 走 HTTP 代理时把请求行写成绝对
// URI 发给代理，所以代理侧能收到完整目标地址。这里只断言「代理被调用了一次」，
// 不看具体报文——报文形状由标准库负责，不是本模块的契约。
func TestGlobalWithProxyGoesThroughProxy(t *testing.T) {
	var proxyHits int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&proxyHits, 1)
		// 代理必须收到绝对 URI（scheme://host/path），这是「走代理」的直接证据。
		if !strings.HasPrefix(r.RequestURI, "http://") {
			t.Errorf("proxy got non-absolute RequestURI %q (request did not go through proxy properly)", r.RequestURI)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"code":0,"msg":"ok","data":{"via":"proxy"}}`)
	}))
	defer proxy.Close()

	var directHits int32
	c := &Client{
		HTTP:          proxyDirectCounter(&directHits, `{"code":0,"msg":"ok","data":{"via":"direct"}}`),
		ChatHTTP:      &http.Client{},
		ChatBaseCN:    "https://chat.example",
		BillingBaseCN: "https://billing.example",
	}
	req, _ := http.NewRequest(http.MethodGet, "http://target.invalid/v2/probe", nil)
	data, err := c.doJSON(proxyGlobalAcct(proxy.URL), req)
	if err != nil {
		t.Fatalf("doJSON via proxy: %v", err)
	}
	if got := atomic.LoadInt32(&proxyHits); got != 1 {
		t.Errorf("proxy hits=%d want 1 (request did not go through the bound proxy)", got)
	}
	if got := atomic.LoadInt32(&directHits); got != 0 {
		t.Errorf("direct hits=%d want 0 (global+proxy must never fall back to direct)", got)
	}
	if !strings.Contains(string(data), `"via":"proxy"`) {
		t.Errorf("body came from wrong path: %s", data)
	}
}

// TestCNWithProxyStillDirect cn 账号即使配了 proxy_addr 也必须直连。
//
// 这是 realm 闸门最重要的一条：cn 账号走国内上游，绕海外节点会把出口 IP 变成
// 可疑来源，等于给风控送特征。闸门在 auth.OutboundProxyAddr 里唯一实现，这里
// 从 upstream 侧再锁一遍——防止有人「优化」成直接读 ProxyAddr 字段。
func TestCNWithProxyStillDirect(t *testing.T) {
	var proxyHits int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&proxyHits, 1)
		io.WriteString(w, `{"code":0,"msg":"ok","data":{}}`)
	}))
	defer proxy.Close()

	var directHits int32
	c := &Client{
		HTTP:          proxyDirectCounter(&directHits, `{"code":0,"msg":"ok","data":{"via":"direct"}}`),
		ChatBaseCN:    "https://chat.example",
		BillingBaseCN: "https://billing.example",
	}
	req, _ := http.NewRequest(http.MethodGet, "https://chat.example/v2/probe", nil)
	if _, err := c.doJSON(proxyCNAcct(proxy.URL), req); err != nil {
		t.Fatalf("doJSON cn: %v", err)
	}
	if got := atomic.LoadInt32(&proxyHits); got != 0 {
		t.Errorf("proxy hits=%d want 0 (cn account must never use a proxy)", got)
	}
	if got := atomic.LoadInt32(&directHits); got != 1 {
		t.Errorf("direct hits=%d want 1", got)
	}
}

// TestGlobalWithoutProxyIsDirect global 账号未分配代理 → 直连，不报错。
func TestGlobalWithoutProxyIsDirect(t *testing.T) {
	var directHits int32
	c := &Client{
		HTTP:          proxyDirectCounter(&directHits, `{"code":0,"msg":"ok","data":{}}`),
		ChatBaseCN:    "https://chat.example",
		BillingBaseCN: "https://billing.example",
	}
	req, _ := http.NewRequest(http.MethodGet, "https://www.workbuddy.ai/v2/probe", nil)
	if _, err := c.doJSON(proxyGlobalAcct(""), req); err != nil {
		t.Fatalf("doJSON global/direct: %v", err)
	}
	if got := atomic.LoadInt32(&directHits); got != 1 {
		t.Errorf("direct hits=%d want 1", got)
	}
}

// TestProxyUnreachableFailsWithoutDirectFallback 代理不可达 → 返回错误，不退回直连。
//
// 这是整套设计的**安全底线**（proxy.on_failure=block 默认）：一个本该走海外住宅 IP
// 的 global 账号静默用机房 IP 打上游，会立刻在上游留下「同机房 IP 多账号」特征，
// 而风控是渐进的——等从 403 里发现时已经很难回溯到「某天代理挂了一小时」。
// 宁可失败（可观测、会熔断、会轮转到别的号），也不要静默改出口身份。
func TestProxyUnreachableFailsWithoutDirectFallback(t *testing.T) {
	var directHits int32
	c := &Client{
		HTTP:          proxyDirectCounter(&directHits, `{"code":0,"msg":"ok","data":{}}`),
		ChatBaseCN:    "https://chat.example",
		BillingBaseCN: "https://billing.example",
	}
	// 127.0.0.1:1 基本不会有人监听 → 连接被拒（快速失败，不会拖慢测试）。
	req, _ := http.NewRequest(http.MethodGet, "http://target.invalid/v2/probe", nil)
	_, err := c.doJSON(proxyGlobalAcct("http://127.0.0.1:1"), req)
	if err == nil {
		t.Fatal("want error when proxy is unreachable, got nil")
	}
	if got := atomic.LoadInt32(&directHits); got != 0 {
		t.Errorf("direct hits=%d want 0 — proxy failure must NOT fall back to direct", got)
	}
}

// TestProxyOnFailureDirectFallsBack 调试开关 proxy.on_failure="direct" 生效：
// 代理**构造失败**时退回直连。
//
// 注意它只覆盖「构造期失败」（地址解析不出来，如漏写 scheme）。代理地址合法但
// 运行期连不通时**不会**回退——那条路径在 hc.Do() 内部，没有 fallback 分支
// （见 TestProxyUnreachableFailsWithoutDirectFallback 与 README 的措辞更正）。
// 所以这个开关不是「本地没起 mihomo 也能跑通」的容灾开关，别照着那个理解去用。
// 它必须被测试覆盖的理由是：如果它其实不生效（比如字段没接线到 upstream.Client），
// 调试的人会以为是代理配错了，浪费在错误的方向上。
func TestProxyOnFailureDirectFallsBack(t *testing.T) {
	var directHits int32
	c := &Client{
		HTTP:           proxyDirectCounter(&directHits, `{"code":0,"msg":"ok","data":{"via":"direct"}}`),
		ChatBaseCN:     "https://chat.example",
		BillingBaseCN:  "https://billing.example",
		ProxyOnFailure: "direct",
	}
	// 缺 scheme → 构造必然失败，正好触发回落分支（不需要真的连一个坏代理）。
	req, _ := http.NewRequest(http.MethodGet, "https://chat.example/v2/probe", nil)
	data, err := c.doJSON(proxyGlobalAcct("127.0.0.1:17891"), req)
	if err != nil {
		t.Fatalf("proxy.on_failure=direct should fall back, got error: %v", err)
	}
	if got := atomic.LoadInt32(&directHits); got != 1 {
		t.Errorf("direct hits=%d want 1 (fallback did not happen)", got)
	}
	if !strings.Contains(string(data), `"via":"direct"`) {
		t.Errorf("unexpected body: %s", data)
	}
}

// TestProxyOnFailureBlockIsDefault 空值等价 block：不显式配置时绝不回落直连。
//
// 与上一条成对：证明「什么都不配」和「配 direct」是两种不同行为，默认值没有
// 被调试开关污染。
func TestProxyOnFailureBlockIsDefault(t *testing.T) {
	var directHits int32
	c := &Client{
		HTTP:          proxyDirectCounter(&directHits, `{"code":0,"msg":"ok","data":{}}`),
		ChatBaseCN:    "https://chat.example",
		BillingBaseCN: "https://billing.example",
		// ProxyOnFailure 留空 = block
	}
	req, _ := http.NewRequest(http.MethodGet, "https://chat.example/v2/probe", nil)
	if _, err := c.doJSON(proxyGlobalAcct("127.0.0.1:17891"), req); err == nil {
		t.Fatal("empty ProxyOnFailure must behave as block (error), not fall back")
	}
	if got := atomic.LoadInt32(&directHits); got != 0 {
		t.Errorf("direct hits=%d want 0", got)
	}
}

// TestInvalidProxyAddrFailsFast 地址写错 → 明确报错，不静默直连。
//
// 两种写法都要拦（都来自人手/脚本拼错），且都不许退回直连：
//   - "127.0.0.1:17891"（缺 scheme）：url.Parse 直接报 "first path segment in URL
//     cannot contain colon"；
//   - "http://"（有 scheme 无 host）：url.Parse 不报错，但 Host 为空，会构造出一个
//     连不上任何地方的代理，症状是全部请求超时而不是明确失败——这种更难排查，
//     newTransportForProxy 专门拦它。
func TestInvalidProxyAddrFailsFast(t *testing.T) {
	cases := []struct{ name, addr string }{
		{"missing scheme", "127.0.0.1:17891"},
		{"missing host", "http://"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var directHits int32
			c := &Client{
				HTTP:          proxyDirectCounter(&directHits, `{"code":0,"msg":"ok","data":{}}`),
				ChatBaseCN:    "https://chat.example",
				BillingBaseCN: "https://billing.example",
			}
			req, _ := http.NewRequest(http.MethodGet, "http://target.invalid/v2/probe", nil)
			_, err := c.doJSON(proxyGlobalAcct(tc.addr), req)
			if err == nil {
				t.Fatalf("want error for bad proxy addr %q, got nil", tc.addr)
			}
			// 错误信息必须点出问题地址，否则运维只看到「请求全挂」无从下手。
			if !strings.Contains(err.Error(), tc.addr) {
				t.Errorf("error should quote the bad addr %q, got: %v", tc.addr, err)
			}
			if got := atomic.LoadInt32(&directHits); got != 0 {
				t.Errorf("direct hits=%d want 0 — bad proxy addr must NOT fall back to direct", got)
			}
		})
	}
}

// TestProxyClientsPooledPerAddr 同一地址复用同一对 client，不同地址各自独立。
//
// 池的键是地址字符串：同地址 = 同一个 listener = 同一个出口，共享连接池是对的；
// 不同地址必须分开，否则两个账号的连接会串到同一个出口上（正是要避免的事）。
func TestProxyClientsPooledPerAddr(t *testing.T) {
	c := &Client{
		HTTP:          &http.Client{},
		ChatBaseCN:    "https://chat.example",
		BillingBaseCN: "https://billing.example",
	}
	a1, err := c.proxyClientFor("http://127.0.0.1:17891")
	if err != nil {
		t.Fatalf("build proxy client 1: %v", err)
	}
	a2, err := c.proxyClientFor("http://127.0.0.1:17891")
	if err != nil {
		t.Fatalf("build proxy client 2: %v", err)
	}
	if a1 != a2 {
		t.Error("same proxy addr must reuse the same client pair")
	}
	b, err := c.proxyClientFor("http://127.0.0.1:17892")
	if err != nil {
		t.Fatalf("build proxy client 3: %v", err)
	}
	if a1 == b {
		t.Error("different proxy addr must get a different client pair")
	}
	// 同一出口内短 RPC 与聊天必须共享 Transport（连接池不重复），且超时档位不同。
	if a1.http.Transport != a1.chatHTTP.Transport {
		t.Error("http and chatHTTP of one proxy must share the same Transport")
	}
	if a1.chatHTTP.Timeout != 0 {
		t.Errorf("proxy chatHTTP.Timeout=%v want 0 (no total cap)", a1.chatHTTP.Timeout)
	}
}

// TestProxyTransportKeepsHardening 代理 transport 与直连 transport 参数逐字一致。
//
// 代理只换出口 IP，不该改协议栈：禁 h2 / 首字节超时 / 连接池容量都必须保持，
// 否则走代理的账号会悄悄失去半死连接防护（这类漂移很难在线上看出来）。
func TestProxyTransportKeepsHardening(t *testing.T) {
	c := &Client{HTTP: &http.Client{}}
	pc, err := c.proxyClientFor("http://127.0.0.1:17891")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	tr, ok := pc.http.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport type=%T", pc.http.Transport)
	}
	if tr.Proxy == nil {
		t.Fatal("proxy transport must have Proxy set")
	}
	if tr.TLSNextProto == nil {
		t.Fatal("TLSNextProto must be non-nil empty map to disable HTTP/2")
	}
	if len(tr.TLSNextProto) != 0 {
		t.Errorf("TLSNextProto must be empty, got %d entries", len(tr.TLSNextProto))
	}
	if tr.ResponseHeaderTimeout != responseHeaderTimeout {
		t.Errorf("ResponseHeaderTimeout=%v want %v", tr.ResponseHeaderTimeout, responseHeaderTimeout)
	}
	// TLSHandshakeTimeout 单独断言：它是「握手阶段卡死」的唯一护栏（对端 SYN 通、
	// TLS 不回话时，只有它会掐断），而 Transport 的其余超时都在握手之后才生效。
	// 漏了这条，把代理分支的握手超时改成 99s 也不会有任何测试变红——直连侧
	// client_test.go 有这条断言，代理侧同样不能少。
	if tr.TLSHandshakeTimeout != tlsHandshakeTimeout {
		t.Errorf("TLSHandshakeTimeout=%v want %v", tr.TLSHandshakeTimeout, tlsHandshakeTimeout)
	}
	if tr.MaxIdleConns != maxIdleConns || tr.MaxIdleConnsPerHost != maxIdleConnsPerHost {
		t.Errorf("pool sizes drifted: %d/%d", tr.MaxIdleConns, tr.MaxIdleConnsPerHost)
	}
	if tr.IdleConnTimeout != idleConnTimeout {
		t.Errorf("IdleConnTimeout=%v want %v", tr.IdleConnTimeout, idleConnTimeout)
	}
}

// TestDirectTransportProxyNil 直连 transport 的 Proxy 必须为 nil。
//
// Proxy 为 nil 时 Go **不读** HTTP_PROXY/HTTPS_PROXY 环境变量——这是「进程级
// 环境变量永远不会把所有账号塞进同一个出口」的技术前提。写成 http.ProxyURL(nil)
// 之类「非 nil 但返回 nil」的形式虽然行为相同，但回读断言就再也看不出区别了。
func TestDirectTransportProxyNil(t *testing.T) {
	tr := newTransport()
	if tr.Proxy != nil {
		t.Error("direct transport must have Proxy == nil (so env vars are ignored)")
	}
}

// TestChatWithProxyGoesThroughProxy 聊天 SSE 也必须走账号绑定的代理。
//
// 这是本文件**最重要**的一条。`chatClientFor` 与 `clientFor` 是两条独立解析路径，
// 只测 doJSON（短 RPC）完全覆盖不到聊天——曾经的真实事故形态是：把 chatClientFor
// 改成恒返回直连，聊天正文全部静默走机房 IP，而仓库里所有测试依然全绿（短 RPC
// 仍走代理，billing 也正常，只有聊天悄悄换了出口）。泄漏的是聊天正文，不是元数据，
// 所以这条不能靠「短 RPC 测过就够了」来省。
//
// 断言两件事：
//  1. 假代理收到请求、直连 client 一次都没被调用；
//  2. chatClientFor 返回的 client 其 Transport 就是代理池里那个对象——防止有人
//     「解析了代理 client 但 Do 的时候又换回直连」。
func TestChatWithProxyGoesThroughProxy(t *testing.T) {
	var proxyHits int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&proxyHits, 1)
		// 走 HTTP 代理时请求行是绝对 URI，与 doJSON 那条测试同款证据。
		if !strings.HasPrefix(r.RequestURI, "http://") {
			t.Errorf("proxy got non-absolute RequestURI %q", r.RequestURI)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"ok\":1}\n\n")
	}))
	defer proxy.Close()

	var directHits int32
	c := &Client{
		HTTP:           proxyDirectCounter(&directHits, `{}`),
		ChatHTTP:       proxyDirectCounter(&directHits, `{}`),
		ChatBaseCN:     "https://chat.example",
		ChatBaseGlobal: "http://target.invalid",
		BillingBaseCN:  "https://billing.example",
		GlobalEnabled:  true,
	}
	a := proxyGlobalAcct(proxy.URL)

	// 先单独锁「解析出来的 client 属于代理池」：这一层不依赖网络，能直接定位
	// 「解析对但用错」的问题。
	hc, err := c.chatClientFor(a)
	if err != nil {
		t.Fatalf("chatClientFor: %v", err)
	}
	if hc == nil {
		t.Fatal("chatClientFor returned nil client for global+proxy account")
	}
	if hc.Transport == nil {
		t.Fatal("chat client has nil Transport")
	}
	// 先比 *http.Client 指针（可比较），再比 Transport：直接比 Transport 在变异体
	// 下会撞上不可比较的 rtFunc 而 panic，把「测试失败」变成「测试崩掉」，掩盖真正
	// 的断言。指针不同就直接停下，也省掉无意义的下一次比较。
	if hc == c.chatHTTP() {
		t.Fatal("chat client IS the direct client — chat would leak to the machine IP")
	}
	pc, err := c.proxyClientFor(proxy.URL)
	if err != nil {
		t.Fatalf("proxyClientFor: %v", err)
	}
	if hc != pc.chatHTTP {
		t.Fatalf("chat client is not the pooled proxy client for this addr")
	}
	if hc.Transport != pc.chatHTTP.Transport {
		t.Errorf("chat client transport is not the pooled proxy transport")
	}

	rc, status, _, err := c.ChatStreamContext(context.Background(), a, []byte(`{"model":"m"}`), "", ChatMeta{ConversationRequestID: "r"})
	if err == nil {
		io.Copy(io.Discard, rc)
		rc.Close()
	}
	if got := atomic.LoadInt32(&proxyHits); got != 1 {
		t.Errorf("proxy hits=%d want 1 (chat did not go through the bound proxy)", got)
	}
	if got := atomic.LoadInt32(&directHits); got != 0 {
		t.Errorf("direct hits=%d want 0 (chat must never fall back to the machine IP)", got)
	}
	if status != 200 {
		t.Errorf("status=%d want 200", status)
	}
}

// TestProxyOKUpstream4xxIsNotAProxyError 代理连得通、上游自己返回 4xx → 照常分类。
//
// 代理这一层新增了两类失败（连不上代理、代理拒绝 CONNECT），它们与「上游返回 403」
// 必须分得清：前者说明出口坏了（不该罚号，换代理/报障），后者说明账号被风控
// （该罚号、该轮转）。混在一起会让风控 403 被当成网络抖动忽略，或者反过来把
// 代理故障算到账号头上。
//
// 这条锁住的是「代理通 + 4xx 走原有 Classify 路径」：返回 *Error（Kind=ErrClient、
// Status=403），不是普通 error。
func TestProxyOKUpstream4xxIsNotAProxyError(t *testing.T) {
	var proxyHits int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&proxyHits, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `{"code":403,"msg":"forbidden by risk control"}`)
	}))
	defer proxy.Close()

	var directHits int32
	c := &Client{
		HTTP:          proxyDirectCounter(&directHits, `{"code":0,"msg":"ok","data":{}}`),
		ChatBaseCN:    "https://chat.example",
		BillingBaseCN: "https://billing.example",
	}
	req, _ := http.NewRequest(http.MethodGet, "http://target.invalid/v2/probe", nil)
	_, err := c.doJSON(proxyGlobalAcct(proxy.URL), req)
	if err == nil {
		t.Fatal("want error for upstream 403, got nil")
	}
	if got := atomic.LoadInt32(&proxyHits); got != 1 {
		t.Errorf("proxy hits=%d want 1", got)
	}
	if got := atomic.LoadInt32(&directHits); got != 0 {
		t.Errorf("direct hits=%d want 0", got)
	}
	ue, ok := err.(*Error)
	if !ok {
		t.Fatalf("upstream 403 must surface as *upstream.Error (so handler can penalise), got %T: %v", err, err)
	}
	if ue.Status != http.StatusForbidden {
		t.Errorf("Status=%d want 403", ue.Status)
	}
	if ue.Kind != ErrClient {
		t.Errorf("Kind=%v want ErrClient (proxy worked; this is the upstream judging the account)", ue.Kind)
	}
}
