package server

import (
	"sync"
	"time"
)

// 降级退避阶梯：首次 30m，之后逐次翻倍，封顶 8h。
//
// 为什么不再「一次误拦等到次日 00:00」：历史上 6 次降级全部来自 cn 账号，但
// global 跟着被替换到次日 00:00，累计降级占 42% 的时间。一次误报要付一整天的
// 代价，恢复边界完全由时钟（零点）决定，与事故是否真的持续无关。
//
// 改成阶梯后：窗口到期即自动放行一个真实请求试探（400 不计费，见
// upstream.Classify 的 ErrContentBlocked 分支不罚号），试探成功就
// ResetBackoff 归零；仍被拦则翻倍。事故持续才会一路升到 8h 封顶。
const (
	degradeBackoffInitial = 30 * time.Minute
	degradeBackoffMax     = 8 * time.Hour
)

// degradeGate 单个 realm 的降级状态机：passthrough/append 模式下请求被上游内容
// 策略拦截时，切换到 Degraded 中性提示词，直到退避窗口到期或用真实请求试探成功。
//
// 定位：误报处理（与 internal/upstream/sanitize.go 同哲学）——内容拦截多为
// system 来源的指纹误报，换最小中性提示词即可绕开；不是对抗框架。
//
// 状态机：进程内存、重启清零（可接受：重启极少触发，且 custom 模式根本
// 不进降级路径）。降级期内的请求直达 Degraded，不再先撞 400。
//
// 零值可用：backoff=0 表示「没有正在退避的事故」，首次 Trigger 初始化为 30m。
type degradeGate struct {
	mu       sync.Mutex
	until    time.Time     // 降级期截止；零值 = 未降级
	backoff  time.Duration // 当前退避阶梯；0 = 无事故（下次 Trigger 从 30m 起）
	lastWarn time.Time     // 上次降级期 WARN 时刻（每分钟最多一条，见 noteWarn）

	// now 时间钩子，零值用 time.Now。仅测试注入：退避阶梯与零点封顶的断言
	// 必须能「先让 until 过期」再连续 Trigger，否则第二次起全是 no-op。
	now func() time.Time
}

// clock 取当前时间（尊重测试钩子）。
func (g *degradeGate) clock() time.Time {
	if g.now != nil {
		return g.now()
	}
	return time.Now()
}

// Active 当前是否处于降级期（now < until）。
func (g *degradeGate) Active() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.clock().Before(g.until)
}

// Trigger 触发一次降级，窗口长度沿退避阶梯翻倍。
//
// 开头的 `if now.Before(g.until) { return }` 必须保留：它的语义是「一个窗口内
// 只算一次事故」，不是「不续期逻辑的残留」。删掉会让阶梯被请求量驱动——
// 降级期内每个进来的请求都会把 backoff 翻一倍，几秒钟就冲到 8h 封顶，而事故
// 实际只发生过一次。阶梯只该由「窗口到期后仍被拦」这件事驱动。
//
// until 上界仍是次日 00:00 CST：8h 封顶在 16:00 之后触发会越过零点，零点本是
// 自然的记账日边界，超过它没有意义（且便于运维按自然日对账）。
func (g *degradeGate) Trigger() {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.clock()
	if now.Before(g.until) {
		return // 本窗口已计过一次事故，不重复翻倍、不续期
	}
	if g.backoff <= 0 {
		g.backoff = degradeBackoffInitial
	} else {
		g.backoff *= 2
		if g.backoff > degradeBackoffMax {
			g.backoff = degradeBackoffMax
		}
	}
	until := now.Add(g.backoff)
	if mid := nextMidnightCST(now); until.After(mid) {
		until = mid
	}
	g.until = until
}

// ResetBackoff 撤销降级并把阶梯归零（下一个事故重新从 30m 起）。
//
// backoff 必须归 0 而不是回到 30m：归 30m 的话下次 Trigger 走「已有退避 → 翻倍」
// 分支，一个新事故的第一个窗口就是 60m。归 0 才能让新事故从阶梯第一级开始。
func (g *degradeGate) ResetBackoff() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.backoff = 0
	g.until = time.Time{}
}

// noteWarn 降级期日志限流：距上次 WARN 满一分钟才允许再打一条，返回是否该打。
// lastWarn 与状态机共用一把锁（不需要第二把）。降级期原本一条日志都没有，
// 是这个问题藏了两个月没人发现的主因，所以这里宁可多打一条也不要静默。
func (g *degradeGate) noteWarn() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.clock()
	if !g.lastWarn.IsZero() && now.Sub(g.lastWarn) < time.Minute {
		return false
	}
	g.lastWarn = now
	return true
}

// degradeSet 按 realm 隔离的降级状态集合。零值可用（懒初始化）。
//
// 为什么要按 realm 拆：一个进程一个降级门时，cn 账号撞的误拦会把 global 一起
// 拖进替换态——上游审核策略、system 指纹、账号画像三者都按域不同，事故不共享。
type degradeSet struct {
	mu    sync.Mutex
	gates map[string]*degradeGate // key: "cn" / "global"
}

// forRealm 取（必要时创建）某 realm 的降级门。realm 为空归一化成 "cn"，
// 与 auth.Realm() 的兜底口径一致（裸模型名就是 CN 请求）。
func (s *degradeSet) forRealm(realm string) *degradeGate {
	if realm == "" {
		realm = "cn"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.gates == nil {
		s.gates = map[string]*degradeGate{}
	}
	g := s.gates[realm]
	if g == nil {
		g = &degradeGate{}
		s.gates[realm] = g
	}
	return g
}

// Active 该 realm 是否处于降级期。
func (s *degradeSet) Active(realm string) bool { return s.forRealm(realm).Active() }

// Trigger 触发该 realm 的一次降级。
func (s *degradeSet) Trigger(realm string) { s.forRealm(realm).Trigger() }

// ResetBackoff 撤销该 realm 的降级并把退避阶梯归零。
func (s *degradeSet) ResetBackoff(realm string) { s.forRealm(realm).ResetBackoff() }

// Snapshot 各 realm 的降级状态快照，供 /healthz 与 /metrics。
//
// 恒输出 cn / global 两个键（即使都为 false）：运维要能稳定断言「字段在且为
// false」，而不是「字段没接上」——缺键和 false 在告警规则里分不清。
func (s *degradeSet) Snapshot() map[string]bool {
	out := map[string]bool{"cn": false, "global": false}
	s.mu.Lock()
	gates := make(map[string]*degradeGate, len(s.gates))
	for k, g := range s.gates {
		gates[k] = g
	}
	s.mu.Unlock()
	// 在 s.mu 之外取各 gate 的锁：避免与 forRealm 的加锁顺序交叉。
	for realm, g := range gates {
		out[realm] = g.Active()
	}
	return out
}

// nextMidnightCST 返回 now 之后最近的 Asia/Shanghai 00:00 时刻（纯函数，方便单测）。
//
// 边界语义：
//   - 23:59 → 次日 00:00（几秒后）
//   - 00:00 → 次日 00:00（刚过 00:00，下个零点是次日）
//
// 用固定 +08:00 偏移计算，避免依赖系统时区配置（容器/宿主机时区不确定）。
func nextMidnightCST(now time.Time) time.Time {
	cst := time.FixedZone("CST", 8*60*60)
	// 把 now 转到 CST 视角取当天 00:00，再加一天；若该 00:00 <= now 则再加一天。
	y, m, d := now.In(cst).Date()
	midnight := time.Date(y, m, d, 0, 0, 0, 0, cst)
	for !midnight.After(now) {
		midnight = midnight.Add(24 * time.Hour)
	}
	return midnight
}
