// Package qoderassets 内嵌 Qoder 上游请求体模板（baseprompt.json）。
//
// 模板为 Qoder 官方客户端 agent_chat_generation 请求体的完整骨架，含占位符：
//   - {UUID1}~{UUID5}：请求/会话/业务级 UUID 槽位（每请求独立替换为新 UUID）；
//   - {TIME1}：business.begin_at 的 unix 毫秒时间戳槽位。
//
// 注意：替换占位符之前整份文件不是合法 JSON（{TIME1} 是裸 token），
// 加载方必须先做占位符替换再 json.Unmarshal（见 service 包 qoder_payload.go）。
package qoderassets

import _ "embed"

// BasepromptJSON 是 Qoder 上游请求体模板原始字节（占位符未替换）。
//
//go:embed baseprompt.json
var BasepromptJSON []byte
