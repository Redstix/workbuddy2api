// fold.go 把 system/developer 消息折叠进 user 消息（features.fold_system_messages）。
//
// ⚠️ 本功能已废弃，勿开启（features.fold_system_messages 恒为 false）。开启会必然失败：
// 折叠排在 ensureConsoleSystem 之后（见 client.go ChatStreamContext），后者刚给 global
// 域补上的兜底 system 会被 FoldSystemMessages 紧接着删掉，上游立刻返回
// {"code":"11-128","msg":"first message is not system prompt"}。即「先补后删」。
//
// 下面这段「上游丢弃 system」的背景**已被实测推翻**，保留原文仅供追溯，不要据它
// 判断行为：
//
//   - 真正的机制是降级门（internal/server/degrade.go）：内容被拦一次 → degrade.Trigger()
//     → 到次日 00:00 CST 前所有请求的 system 被 prompt.Rewrite(body, prompt.Degraded)
//     整体替换成那句中性提示词。现象（system 里的指令「看不见」）与「被丢弃」一样，
//     成因相反——所以当年的实测极可能是在降级状态下做的，把「被替换」误判成「被丢弃」。
//   - 上游要求首条必须是 system：ensureConsoleSystem 的存在本身就是这条要求的证据，
//     而 fold 恰好把 system 删光。
//
// 保留代码而不删除的原因：折叠逻辑本身（messageText/mergeIntoContent/顺序合并）是
// 正确且测试完备的，将来若上游真的不认 system 角色，它仍是可复用的底座；删掉则要
// 连测试一起重写。开关保持默认关，等于死代码，不影响任何运行路径。
//
// —— 以下为原始（已过时的）设计说明 ——
//
// 背景：WorkBuddy 上游（CN /v2 与 global 同源）只转发 user/assistant 内容，
// **role=system 的消息被静默丢弃**——实测同一段 markdown 标记文本放进 system
// 时模型完全看不见（1 百到 2.4 万字符各档长度均不可见），同内容放进 user
// 立即可见；role=developer 同样无效。这使所有「把约定写在 system 里」的客户端
// 在上游侧等于没有系统提示词：Claude Code / Codex 的项目规范、以及 OpenViking
// 这类把输出契约（数万字符 DSL 规范）放在 system 的提取流程全部失效
// （表现为提取模型无视契约、parse_error 反复重试直至输出预算耗尽）。
//
// 方案：出站前把 system/developer 的**文本内容**按原顺序拼接，前置合并进第一条
// user 消息，然后删除这些 system/developer 消息。语义等价于客户端把指令写进
// 用户消息——上游能看到，指令不丢。
//
// 与既有 prompt 三模式的关系：正交。prompt.mode 决定「用哪段系统提示词」
// （透传客户端 / 网关替换 / 追加），本开关决定「系统提示词以什么角色送出」。
// 因此 passthrough+custom+append 三种模式都可叠加折叠；降级期（Degraded 中性
// 提示词）同样生效——折叠发生在每次出站尝试前的最后一环，降级重写后的
// system 会被一并折叠。
//
// 开关默认关闭：折叠会改变请求形态（system 不再出现），对只做普通对话的
// 客户端没有收益，按需开启。
package upstream

import (
	"encoding/json"
	"log"
	"strings"
)

// foldableRoles 判定哪些角色属于「必须折叠」的系统提示词角色。
//
// developer 是 OpenAI 新规范里 system 的别名（Codex / Cursor 等用它承载
// system 级指令）；normalizeRoles 在管线里已把 developer 归一为 system，
// 这里仍显式同时匹配，使 FoldSystemMessages 单独调用（测试、降级重试路径）
// 时口径与管线内一致。
func foldableRole(role string) bool {
	return role == "system" || role == "developer"
}

// messageText 提取一条消息内容的纯文本。
//
// 两种形态都要认：content 为 string（普通消息），或为 []any 多模态分区
// （[{"type":"text","text":"..."},{"type":"image_url",...}]）。非文本分区
// （图片、音频等）跳过——它们无法并入文本，且在 system 消息里本就罕见。
func messageText(content any) string {
	switch c := content.(type) {
	case string:
		return c
	case []any:
		var b strings.Builder
		for _, raw := range c {
			part, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			typ, _ := part["type"].(string)
			if typ != "text" && typ != "input_text" {
				continue
			}
			text, _ := part["text"].(string)
			if text == "" {
				continue
			}
			if b.Len() > 0 {
				b.WriteString("\n\n")
			}
			b.WriteString(text)
		}
		return b.String()
	}
	return ""
}

// mergeIntoContent 把 block 前置合并进一条消息的 content，保持原有内容形态：
//   - string → block + "\n\n" + 原文；
//   - []any  → 头部插入一个 text 分区（不改变既有分区类型与顺序，图片等仍在原位）；
//   - 缺失/null/其他 → 直接置为 block。
func mergeIntoContent(content any, block string) any {
	switch c := content.(type) {
	case string:
		if strings.TrimSpace(c) == "" {
			return block
		}
		return block + "\n\n" + c
	case []any:
		head := map[string]any{"type": "text", "text": block}
		merged := make([]any, 0, len(c)+1)
		merged = append(merged, head)
		merged = append(merged, c...)
		return merged
	case nil:
		return block
	}
	return block
}

// FoldSystemMessages 把 messages 中所有 system/developer 的文本折叠进第一条 user 消息。
//
// ⚠️ 调用方注意：不要在 ensureConsoleSystem 之后调用（见文件头）。global 域会因此
// 返回 11-128，本功能已废弃。
//
// 行为（对齐已验证的代理实现）：
//   - 折叠顺序 = 原消息顺序，块间以空行分隔；
//   - 合并目标 = **第一条** user 消息（多轮历史只在首条注入一次，后续轮次不重复）；
//   - 无 user 消息时在最前插入一条 {"role":"user","content":block}；
//   - 无可折叠内容（无 system/developer、或内容全为空）→ 原样返回 body；
//   - 非 system/developer 的其他角色（user/assistant/tool/未知值）逐条原样保留，
//     不重排、不改写。
//
// 坏 JSON / 空 body → 原样返回（与 prepareBody 语义一致：出站改写绝不失败）。
func FoldSystemMessages(body []byte) []byte {
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
	hoisted := make([]string, 0, 2)
	rest := make([]any, 0, len(msgs))
	for _, raw := range msgs {
		msg, ok := raw.(map[string]any)
		if !ok {
			rest = append(rest, raw)
			continue
		}
		role, _ := msg["role"].(string)
		if !foldableRole(role) {
			rest = append(rest, raw)
			continue
		}
		if text := strings.TrimSpace(messageText(msg["content"])); text != "" {
			hoisted = append(hoisted, text)
		}
	}
	if len(hoisted) == 0 {
		return body
	}
	block := strings.Join(hoisted, "\n\n")
	merged := false
	for _, raw := range rest {
		msg, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if role, _ := msg["role"].(string); role != "user" {
			continue
		}
		msg["content"] = mergeIntoContent(msg["content"], block)
		merged = true
		break
	}
	if !merged {
		rest = append([]any{map[string]any{"role": "user", "content": block}}, rest...)
	}
	obj["messages"] = rest
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	log.Printf("[upstream] folded %d system message(s) -> %d chars into first user message (total msgs %d->%d)",
		len(hoisted), len(block), len(msgs), len(rest))
	return out
}
