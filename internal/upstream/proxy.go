// proxy.go 账号级出站代理：把**单个账号**的全部上游请求固定送进它自己的本地代理
// listener（mihomo 的 http inbound，如 127.0.0.1:17891），从而让「一个账号一个出口
// IP」在网关这一侧成立。
//
// 为什么需要：global 账号共用机房出口 IP 时，上游风控会把「同一 IP 上的多账号高频
// 请求」当集群特征处理（WAF 403 渐进收紧）。给每个账号分配一个本地 listener 后，
// 每个 listener 后面是 mihomo 里的一条独立线路，上游看到的是各自不同的住宅/移动 IP。
//
// 边界（刻意的设计约束，不要在这里扩展）：
//   - 网关**不做**节点切换、区域回退、测活。它只知道「这个账号走这个地址」，
//     地址后面换了哪条线是 mihomo 的事。网关一旦开始选节点，就得同步维护节点
//     状态机、健康检查、区域映射，而它根本看不到 mihomo 内部的真实选择——那套
//     状态必然与事实脱节。
//   - 网关**不碰**选号逻辑（internal/pool/pick.go）：代理绑定是账号的一个属性，
//     只影响「这个号从哪出去」，不影响「选哪个号」。
//
// 闸门：唯一实现是 auth.Auth.OutboundProxyAddr()——cn 账号永远返回空（直连），
// global 账号才可能拿到地址。本文件所有出口都先过那道闸门，不重复判断 realm。
package upstream

import (
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/logfmt"
)

// proxyOnFailureBlock / proxyOnFailureDirect 是 ProxyOnFailure 的两个取值。
//
// block（默认）：代理不可用 → 该账号的请求直接失败。这是**唯一安全**的默认：
// 一个本该走海外住宅 IP 的 global 账号静默退回机房直连，会立刻在上游留下「同一
// 机房 IP 上的多账号」特征，风控是渐进的——等你从 403 里发现时，已经很难回溯到
// 「是某天代理挂了一小时，这些号悄悄直连过」。宁可失败（失败会熔断/轮转到别的
// 账号，是可观测、可自愈的），也不要静默改变出口身份。
//
// direct：仅供调试（本地没起 mihomo 时想跑通链路）。用它就意味着接受上面那段
// 风险。它对**所有**出口生效（短 RPC 与聊天一致）：只在某一半生效会让「同一个
// 账号的 chat 走代理、billing 直连」这种更难解释的状态出现，调试时反而更迷惑。
const (
	proxyOnFailureBlock  = "block"
	proxyOnFailureDirect = "direct"
)

// proxyClient 一个代理出口上的一对 client：短 RPC（有总时长上限）与聊天 SSE
// （无总时长上限）。两者共享**同一个** *http.Transport 实例，连接池不重复——
// 与直连侧 HTTP/ChatHTTP 的关系逐字对应（见 New()）。
type proxyClient struct {
	http     *http.Client
	chatHTTP *http.Client
}

// directOnProxyFailure 报告代理失败时是否允许退回直连（proxy.on_failure="direct"）。
// 空值 = block（默认）。
func (c *Client) directOnProxyFailure() bool {
	return strings.EqualFold(strings.TrimSpace(c.ProxyOnFailure), proxyOnFailureDirect)
}

// proxyClientFor 取（必要时构造并缓存）addr 对应的 client 对。
//
// 缓存键是代理地址字符串本身：同一地址的账号共享一个连接池（它们本来就该共用——
// 地址相同意味着同一个 listener、同一个出口），不同地址各自独立池。懒构造而非
// 启动时全量建：auth 目录热加载可能随时新增账号，启动时枚举会漏。
//
// 构造失败（地址写错、缺 scheme）返回 error，由调用方决定 block 还是 direct；
// 绝不在这里静默回落直连。
func (c *Client) proxyClientFor(addr string) (*proxyClient, error) {
	// 快路径：绝大多数请求只是读已建好的池（每个上游请求都会经过这里）。
	c.proxyMu.RLock()
	pc, ok := c.proxyClients[addr]
	c.proxyMu.RUnlock()
	if ok {
		return pc, nil
	}

	c.proxyMu.Lock()
	defer c.proxyMu.Unlock()
	// 双重检查：两个 goroutine 可能同时走到慢路径，先到的已经建好了。
	if pc, ok := c.proxyClients[addr]; ok {
		return pc, nil
	}
	tr, err := newTransportForProxy(addr)
	if err != nil {
		return nil, err
	}
	// 与 main.go 对直连 Transport 的运行时覆盖保持一致：聊天首字节上限由
	// config upstream.header_timeout_seconds 决定，代理侧必须同样生效，否则走
	// 代理的账号会退回 transport.go 里 60s 的构造默认，与直连账号行为分叉。
	if c.HeaderTimeout > 0 {
		tr.ResponseHeaderTimeout = c.HeaderTimeout
	}
	// 短 RPC 的总时长上限沿用直连 client 的当前值（main.go 已按
	// upstream.timeout_seconds 覆盖过 c.HTTP.Timeout），不在本文件另立默认——
	// 两套超时并存迟早漂移。聊天 client 恒 0（无总时长，靠首字节+空闲监控）。
	var shortTimeout time.Duration
	if c.HTTP != nil {
		shortTimeout = c.HTTP.Timeout
	}
	built := &proxyClient{
		http:     &http.Client{Timeout: shortTimeout, Transport: tr},
		chatHTTP: &http.Client{Timeout: 0, Transport: tr},
	}
	if c.proxyClients == nil {
		c.proxyClients = make(map[string]*proxyClient)
	}
	c.proxyClients[addr] = built
	return built, nil
}

// clientFor 返回该账号短 RPC 应使用的 client：无代理（cn / global 未分配）→ 直连
// 的 c.HTTP；有代理 → 该出口的 client。
//
// 返回 error 而不是静默回落：见 proxyOnFailureBlock 注释。
func (c *Client) clientFor(a *auth.Auth) (*http.Client, error) {
	addr := a.OutboundProxyAddr()
	if addr == "" {
		return c.HTTP, nil
	}
	pc, err := c.proxyClientFor(addr)
	if err != nil {
		if c.directOnProxyFailure() {
			log.Printf("WARN: [upstream] proxy %q unusable, falling back to direct (proxy.on_failure=direct, DEBUG ONLY): %v", addr, err)
			return c.HTTP, nil
		}
		return nil, fmt.Errorf("proxy %q unusable: %w", addr, err)
	}
	return pc.http, nil
}

// chatClientFor 返回该账号聊天 SSE 应使用的 client。回落语义与 clientFor 完全一致
// （proxy.on_failure 同时作用于聊天与短 RPC，见 proxyOnFailureDirect 注释）。
func (c *Client) chatClientFor(a *auth.Auth) (*http.Client, error) {
	addr := a.OutboundProxyAddr()
	if addr == "" {
		return c.chatHTTP(), nil
	}
	pc, err := c.proxyClientFor(addr)
	if err != nil {
		if c.directOnProxyFailure() {
			log.Printf("WARN: [upstream] proxy %q unusable, chat falls back to direct (proxy.on_failure=direct, DEBUG ONLY): %v", addr, err)
			return c.chatHTTP(), nil
		}
		return nil, fmt.Errorf("proxy %q unusable: %w", addr, err)
	}
	return pc.chatHTTP, nil
}

// proxyLabel 日志用：代理出口标识（空 = 直连）。只用于排障对账，不含凭据
// （proxy_addr 约定是本地 listener 地址，不带 user:pass）。
func proxyLabel(a *auth.Auth) string {
	if addr := a.OutboundProxyAddr(); addr != "" {
		return addr
	}
	return "direct"
}

// logProxyDown 统一记录「哪个账号走了哪条出口、失败原因」，供排障时按 uid8 grep。
func logProxyDown(a *auth.Auth, err error) {
	log.Printf("ERR: [upstream] acct=%s proxy=%s: %v", logfmt.Label(a.UID, a.Nickname), proxyLabel(a), err)
}
