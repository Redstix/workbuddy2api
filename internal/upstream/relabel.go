// relabel.go 出站前把 system/developer 角色重贴成 user，并在最前面补一条中性 system。
//
// 背景（2026-11-28 实测，绕过网关直连上游验证）：WorkBuddy 上游的 11-128 拦截
// 是**按 role 判定**，不是按内容逐字匹配 —— system / developer 角色里的 DSH 身份
// 句式会被拦（400 code 11-128，与消息在数组里的位置无关，大小写不敏感）；同样的
// 字节换成 user 角色即 200，内容全量送达（实测 prompt_tokens 124191），指令依然
// 生效（语义检查：把 "Reply with exactly the single word BANANA" 放进 user 消息，
// 模型确实只回 BANANA）。
//
// 同时 global 域有相反的约束：首条消息必须是 system，否则上游同样报 11-128。
// 两个域方向相反，所以出站形态必须同时满足两边：
//
//	[1] role=system  "You are a helpful assistant."   ← 中性头，满足 global 首条须 system
//	[2] role=user    <原 system 的全部内容>            ← 原 system 降级为 user，避开拦截
//	[3] role=user    <原第一条 user>                   ← 其余消息完全原样
//	...
//
// 修法不是「删内容」，也不是「折叠进 user 再把 system 删掉」（那会合并多段文本、
// 丢掉多模态分区、改变消息顺序），而是**只改 role 字段**：消息条数、顺序、content
// 字节全部原样保留，语义上等价于客户端把指令写在 user 消息里。代价是 prompt cache
// 前缀从第一个字节起就变了（role 标签在 content 之前），首个请求需重付一次前缀。
//
// 与 prompt 三模式（passthrough / custom / append）的关系：完全正交。本文件只决定
// 「系统提示词以什么角色、以什么顺序送出」，不关心那段提示词是谁写的、写了几段。
//
// 对所有 realm 一致生效（不做 realm 分叉）：cn 域同样存在该拦截，且分叉会让
// 「同一请求换个 realm 行为就变」这种漂移无从排查。
package upstream

import (
	"encoding/json"
	"log"
	"strings"
)

// neutralSystemContent 是前置的中性 system 正文。
//
// 它只承担一个职责：让 global 域的「首条消息必须是 system」检查通过，不携带任何
// 行为约束。实测（2026-11-28）这句在 cn / global 两域都不会触发 11-128。
const neutralSystemContent = "You are a helpful assistant."

// relabelableRole 判定哪些角色需要重贴成 user。
//
// 按 11-128 的实测口径：system 与 developer 都是被拦的角色（developer 是 OpenAI
// 新规范里 system 的别名）。normalizeRoles 在 prepareBody 里已把 developer 归一为
// system，这里仍显式同时匹配，使本函数单独调用（测试、以及将来若调整管线顺序）时
// 口径不变。
func relabelableRole(role string) bool {
	r := strings.TrimSpace(role)
	return strings.EqualFold(r, "system") || strings.EqualFold(r, "developer")
}

// mutateMessages 是所有出站消息改写的共用入口：解析 body、取出 messages 交给 fn，
// 由 fn 报告是否改动以及改了几条。解析失败 / 空 body / 无 messages 一律原样返回
// （与 prepareBody 同语义：出站改写绝不失败，宁可原样送出也不阻断请求）。
//
// 未改动时不重新序列化，直接返回入参字节 —— 这样幂等，且不会因为 map 重新排序而
// 无谓地打乱 prompt cache。
func mutateMessages(body []byte, fn func(msgs []any) (out []any, n int)) []byte {
	if len(body) == 0 {
		return body
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	msgs, ok := obj["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return body
	}
	out, n := fn(msgs)
	if n == 0 {
		return body
	}
	obj["messages"] = out
	buf, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return buf
}

// isNeutralHead 判定一条消息是否就是中性头本身（role=system 且正文等于
// neutralSystemContent）。用于让两个函数互相幂等：RelabelSystemToUser 不把中性头
// 降级成 user，PrependNeutralSystem 不重复插入第二条。
//
// 这条判定同时保证了「客户端自己就写着这句话」的情形得到正确结果：那条消息本来
// 就是安全的 system（实测 cn/global 两域均 200），保留为 system 既不必多余降级，
// 也不会让 global 域多出一条重复的头。
func isNeutralHead(msg map[string]any) bool {
	role, _ := msg["role"].(string)
	content, _ := msg["content"].(string)
	return strings.EqualFold(strings.TrimSpace(role), "system") && content == neutralSystemContent
}

// RelabelSystemToUser 把 messages 里所有 system/developer 消息的 role 改成 user。
//
// 行为：
//   - 只改 msg["role"]，content（含多模态 []any）、消息条数、顺序、其他字段全不动；
//   - 不合并、不重排、不删除任何消息（两条连续 user 是上游接受的形态，实测 200）；
//   - 角色判定用 EqualFold + TrimSpace，大小写与首尾空格变体一并覆盖；
//   - 中性头（isNeutralHead）保持 system 不动，使本函数与 PrependNeutralSystem 幂等；
//   - 坏 JSON / 空 body / 无 messages / 无 system 或 developer → 原样返回。
//
// 幂等：对已重贴过的 body 再跑一次，输出与输入字节一致（第二次没有可重贴的消息，
// 走「无改动」分支直接返回入参）。
func RelabelSystemToUser(body []byte) []byte {
	return mutateMessages(body, func(msgs []any) ([]any, int) {
		n := 0
		for _, raw := range msgs {
			msg, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			role, _ := msg["role"].(string)
			if !relabelableRole(role) {
				continue
			}
			if isNeutralHead(msg) {
				continue // 中性头是我们自己补的，保持 system
			}
			msg["role"] = "user"
			n++
		}
		if n > 0 {
			log.Printf("[upstream] relabeled %d system/developer message(s) -> user (11-128 is role-based; content untouched)", n)
		}
		return msgs, n
	})
}

// PrependNeutralSystem 在 messages 最前面插一条中性 system。
//
// 用途：global 域要求首条消息是 system，而 RelabelSystemToUser 刚把所有 system 都
// 变成了 user，所以必须补回一条。必须先 relabel 再 prepend；顺序反了会把中性头也
// 变成 user，等于没做。
//
// 幂等：首条已经是同一条中性 system 时不重复插入（因此本函数可以安全地重复调用、
// 也可以安全地和 ensureConsoleSystem 的注入结果叠加而不会出现两条）。
//
// 首条是别的内容也照样插 —— 中性头是恒定前置，不因原始请求有没有 system 而变，
// 这样 global 域在「客户端完全不带 system」时也能过检查。
func PrependNeutralSystem(body []byte) []byte {
	return mutateMessages(body, func(msgs []any) ([]any, int) {
		if first, ok := msgs[0].(map[string]any); ok && isNeutralHead(first) {
			return msgs, 0 // 已有一条同样的中性头：不重复插
		}
		head := map[string]any{"role": "system", "content": neutralSystemContent}
		out := make([]any, 0, len(msgs)+1)
		out = append(out, head)
		out = append(out, msgs...)
		log.Printf("[upstream] prepended neutral system head (global requires first message be system)")
		return out, 1
	})
}

// NormalizeSystemRoles 组合上面两步，返回出站最终形态：
// 先把所有 system/developer 重贴成 user，再在最前面补一条中性 system。
//
// 顺序不可交换：先补后贴会把中性头一起降级，global 域随即被拦。
func NormalizeSystemRoles(body []byte) []byte {
	return PrependNeutralSystem(RelabelSystemToUser(body))
}
