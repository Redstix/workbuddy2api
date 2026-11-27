package server

import (
	"testing"
	"time"
)

// TestNextMidnightCSTBoundaries 边界：CST 视角下当日 00:00 之后 → 次日 00:00，
// 23:59 → 次日 00:00，正午 → 次日 00:00。用固定 +08:00 偏移计算，不依赖系统时区。
func TestNextMidnightCSTBoundaries(t *testing.T) {
	cases := []struct {
		name string
		now  time.Time
		// 期望 until 的 CST 时分秒恒为 00:00:00，且 strictly after now。
	}{
		{"23:59 -> next 00:00", time.Date(2026, 9, 11, 23, 59, 0, 0, cstZone)},
		{"00:00:01 -> next 00:00", time.Date(2026, 9, 11, 0, 0, 1, 0, cstZone)},
		{"12:00 -> next 00:00", time.Date(2026, 9, 11, 12, 0, 0, 0, cstZone)},
		{"00:00:00 exact -> next 00:00", time.Date(2026, 9, 11, 0, 0, 0, 0, cstZone)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := nextMidnightCST(tc.now)
			if !got.After(tc.now) {
				t.Fatalf("until %v not after now %v", got, tc.now)
			}
			// CST 视角下必须落在 00:00:00。
			if g := got.In(cstZone); g.Hour() != 0 || g.Minute() != 0 || g.Second() != 0 {
				t.Errorf("until CST = %02d:%02d:%02d, want 00:00:00", g.Hour(), g.Minute(), g.Second())
			}
		})
	}
}

// TestNextMidnightCSTCrossMonth 跨月：月末 23:59 → 次月 1 日 00:00。
func TestNextMidnightCSTCrossMonth(t *testing.T) {
	now := time.Date(2026, 1, 31, 23, 59, 0, 0, cstZone)
	got := nextMidnightCST(now)
	want := time.Date(2026, 2, 1, 0, 0, 0, 0, cstZone)
	if !got.Equal(want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

// fakeClock 可推进的测试时钟，供退避阶梯/零点封顶断言「先让 until 过期」。
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

// newGateWithClock 造一个注入假时钟的降级门（10:00 CST 起点：离零点足够远，
// 前几级阶梯不会被 nextMidnightCST 截断，断言只反映阶梯本身）。
func newGateWithClock() (*degradeGate, *fakeClock) {
	c := &fakeClock{t: time.Date(2026, 3, 2, 10, 0, 0, 0, cstZone)}
	return &degradeGate{now: c.now}, c
}

// gateState 读 (backoff, until) 快照（持锁，避免与状态机并发读）。
func gateState(g *degradeGate) (time.Duration, time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.backoff, g.until
}

// TestDegradeGateActiveTriggered Active() 在 Trigger 后为 true，过期后为 false。
func TestDegradeGateActiveTriggered(t *testing.T) {
	g, clk := newGateWithClock()
	if g.Active() {
		t.Error("fresh gate should not be active")
	}
	g.Trigger()
	if !g.Active() {
		t.Error("after Trigger should be active")
	}
	// 窗口到期（推进到 until 之后）→ 自动失效，不需要任何人调 Reset。
	clk.advance(degradeBackoffInitial + time.Second)
	if g.Active() {
		t.Error("after window expiry should not be active")
	}
}

// TestDegradeGateTriggerNoRenewal 降级期内再次 Trigger 什么都不变
// （until 不变、backoff 不变）——「一个窗口只算一次事故」。
//
// 这条语义不是「不续期」的遗留物，而是退避阶梯的正确性前提：若不拦住，
// 降级期内每个进来的请求都会把 backoff 翻一倍，几秒就冲到 8h 封顶。
func TestDegradeGateTriggerNoRenewal(t *testing.T) {
	g, _ := newGateWithClock()
	g.Trigger()
	firstBackoff, firstUntil := gateState(g)

	g.Trigger() // 仍在窗口内
	secondBackoff, secondUntil := gateState(g)

	if !firstUntil.Equal(secondUntil) {
		t.Errorf("Trigger during active should not renew until: first=%v second=%v", firstUntil, secondUntil)
	}
	if firstBackoff != secondBackoff {
		t.Errorf("Trigger during active should not double backoff: first=%v second=%v", firstBackoff, secondBackoff)
	}
}

// TestDegradeGateBackoffLadder 退避阶梯：每次「窗口到期后仍被拦」翻一倍，
// 30m → 1h → 2h。必须先让 until 过期再 Trigger，否则第二次起全是 no-op。
func TestDegradeGateBackoffLadder(t *testing.T) {
	g, clk := newGateWithClock()
	want := []time.Duration{30 * time.Minute, time.Hour, 2 * time.Hour}
	for i, w := range want {
		g.Trigger()
		backoff, until := gateState(g)
		if backoff != w {
			t.Fatalf("trigger #%d: backoff=%v want %v", i+1, backoff, w)
		}
		if got, exp := until, clk.now().Add(w); !got.Equal(exp) {
			t.Fatalf("trigger #%d: until=%v want now+%v=%v", i+1, got, w, exp)
		}
		// 让窗口过期，下一次 Trigger 才会计为新事故。
		clk.advance(w + time.Second)
	}
}

// TestDegradeGateBackoffCapped 连续 20 次「窗口到期后仍被拦」：backoff 封顶 8h，
// until 永不越过次日 00:00 CST。
func TestDegradeGateBackoffCapped(t *testing.T) {
	g, clk := newGateWithClock()
	for i := 0; i < 20; i++ {
		g.Trigger()
		backoff, until := gateState(g)
		if backoff > degradeBackoffMax {
			t.Fatalf("trigger #%d: backoff=%v exceeds cap %v", i+1, backoff, degradeBackoffMax)
		}
		if mid := nextMidnightCST(clk.now()); until.After(mid) {
			t.Fatalf("trigger #%d: until=%v beyond next midnight %v", i+1, until, mid)
		}
		clk.advance(backoff + time.Second)
	}
	backoff, _ := gateState(g)
	if backoff != degradeBackoffMax {
		t.Fatalf("after 20 triggers backoff=%v want %v", backoff, degradeBackoffMax)
	}
}

// TestDegradeGateUntilClampedToMidnight 23:50 触发：30m 窗口本应到次日 00:20，
// 被 nextMidnightCST 截到 00:00（零点既是记账日边界，越过无意义）。
func TestDegradeGateUntilClampedToMidnight(t *testing.T) {
	clk := &fakeClock{t: time.Date(2026, 3, 2, 23, 50, 0, 0, cstZone)}
	g := &degradeGate{now: clk.now}
	g.Trigger()
	_, until := gateState(g)
	want := time.Date(2026, 3, 3, 0, 0, 0, 0, cstZone)
	if !until.Equal(want) {
		t.Fatalf("until=%v want clamped to %v", until, want)
	}
	// backoff 本身仍是第一级（钳制只影响 until，不改变下次翻倍的基准）。
	backoff, _ := gateState(g)
	if backoff != degradeBackoffInitial {
		t.Fatalf("backoff=%v want %v", backoff, degradeBackoffInitial)
	}
}

// TestDegradeGateResetBackoff Trigger 后 Reset：Active 变 false、backoff 归 0；
// 再 Trigger 一次必须从 30m 重新起阶梯（不是 60m）。
//
// 这是「ResetBackoff 归 0 而不是归 30m」的核心验证：若归 30m，下次 Trigger 走
// 「已有退避 → 翻倍」分支，新事故的第一个窗口就是 60m。
func TestDegradeGateResetBackoff(t *testing.T) {
	g, _ := newGateWithClock()
	g.Trigger()
	g.Trigger()
	if g.Active() == false {
		t.Fatal("precondition: gate should be active after Trigger")
	}

	g.ResetBackoff()
	if g.Active() {
		t.Error("after ResetBackoff should not be active")
	}
	if backoff, until := gateState(g); backoff != 0 || !until.IsZero() {
		t.Errorf("after ResetBackoff backoff=%v until=%v want 0 and zero time", backoff, until)
	}

	g.Trigger()
	if backoff, _ := gateState(g); backoff != degradeBackoffInitial {
		t.Errorf("first trigger after reset backoff=%v want %v (new incident starts at step 1)", backoff, degradeBackoffInitial)
	}
}

// TestDegradeGateNoteWarnThrottled 降级期 WARN 每分钟最多一条（降级期原本零日志，
// 是这个问题藏两个月的主因；限流只为不刷屏，不是不报）。
func TestDegradeGateNoteWarnThrottled(t *testing.T) {
	g, clk := newGateWithClock()
	if !g.noteWarn() {
		t.Fatal("first warn should be allowed")
	}
	clk.advance(30 * time.Second)
	if g.noteWarn() {
		t.Error("second warn within a minute should be throttled")
	}
	clk.advance(31 * time.Second)
	if !g.noteWarn() {
		t.Error("warn after a minute should be allowed again")
	}
}

// TestDegradeSetRealmIsolation 核心修复：cn 触发降级，global 必须完全不受影响。
// 这是「6 次降级全部来自 cn，global 跟着被替换到次日 00:00、累计 42% 时间」的回归锁。
func TestDegradeSetRealmIsolation(t *testing.T) {
	var s degradeSet
	if s.Active("cn") || s.Active("global") {
		t.Fatal("fresh set should have nothing active")
	}
	s.Trigger("cn")
	if !s.Active("cn") {
		t.Fatal("cn should be active after Trigger(cn)")
	}
	if s.Active("global") {
		t.Fatal("global must NOT be affected by a cn trigger (cross-realm contamination)")
	}
	// 反向：global 触发不影响 cn。
	s.Trigger("global")
	if !s.Active("global") {
		t.Fatal("global should be active after Trigger(global)")
	}
	// cn 先 Reset，global 的窗口不应被动到。
	s.ResetBackoff("cn")
	if s.Active("cn") {
		t.Error("cn should be reset")
	}
	if !s.Active("global") {
		t.Error("resetting cn must not reset global")
	}
}

// TestDegradeSetEmptyRealmNormalized realm 为空归一化成 "cn"（与 auth.Realm() 兜底
// 口径一致：裸模型名就是 CN 请求）。手搓 degradeSet 的调用方不会传 ""，但归一化
// 必须成立，否则 "" 会自成一个 realm、与 "cn" 各持一份状态。
func TestDegradeSetEmptyRealmNormalized(t *testing.T) {
	var s degradeSet
	s.Trigger("")
	if !s.Active("cn") {
		t.Error(`Trigger("") should activate realm "cn"`)
	}
	if s.Active("global") {
		t.Error(`Trigger("") must not touch "global"`)
	}
	// "" 与 "cn" 必须是同一个 gate（同一份状态），不是各自一份。
	s.ResetBackoff("cn")
	if s.Active("") {
		t.Error(`ResetBackoff("cn") should also clear realm "" (same gate)`)
	}
}

// TestDegradeSetSnapshot /healthz 与 /metrics 靠它取状态：恒输出 cn/global 两个键
// （即使都 false——缺键和 false 在告警规则里分不清），且各 realm 状态独立。
func TestDegradeSetSnapshot(t *testing.T) {
	var s degradeSet
	snap := s.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("snapshot should always carry both realms, got %v", snap)
	}
	if _, ok := snap["cn"]; !ok {
		t.Error("snapshot missing cn key")
	}
	if _, ok := snap["global"]; !ok {
		t.Error("snapshot missing global key")
	}
	if snap["cn"] || snap["global"] {
		t.Errorf("fresh snapshot should be all false, got %v", snap)
	}

	s.Trigger("global")
	snap = s.Snapshot()
	if snap["cn"] {
		t.Error("cn should stay false")
	}
	if !snap["global"] {
		t.Error("global should be true after Trigger(global)")
	}
}

// TestDegradeSetForRealmSameGate 同一 realm 反复取必须是同一个 gate 指针，
// 否则每次调用都会造一份新状态、降级期永远不生效。
func TestDegradeSetForRealmSameGate(t *testing.T) {
	var s degradeSet
	if s.forRealm("cn") != s.forRealm("cn") {
		t.Error("forRealm must return a stable gate per realm")
	}
	if s.forRealm("cn") == s.forRealm("global") {
		t.Error("cn and global must be distinct gates")
	}
	if s.forRealm("") != s.forRealm("cn") {
		t.Error(`forRealm("") must be the same gate as forRealm("cn")`)
	}
}
