package service

// upstream_notice.go 上游「面向模型的护栏/系统提示」文本处理。
//
// 背景：管理端有几条链路会把上游响应体片段直接拼进给管理员看的错误信息（模型目录
// 同步失败原因、账号状态 error_message、连接测试提示）。部分上游（尤其自研网关与
// 聚合站）在被请求到它不支持的能力时，不返回结构化 error，而是把一段**写给模型看
// 的指令文本**当作正常内容回吐，典型形态：
//
//	[System reminder: the current model does not support images. Content filtered.
//	 Please inform user to switch to a multimodal model or try alternative approach.]
//
// 这类文本有两个特征让它在管理端很难看：多行、长度几十到上百字符，且不含任何管理员
// 可据以行动的信息。本文件只做两件保守的事：
//  1. flattenUpstreamSnippet：把换行/制表/连续空白压成单空格 —— 保证 toast 与表格
//     单元格单行可读，不改变任何字符内容（原文照旧可排障，不做脱敏式改写，避免把
//     真实失败原因一起吃掉）；
//  2. isUpstreamGuardrailNotice：识别「整段只是护栏提示」，供调用方决定不要把这种
//     回复当作能力证据（见 openai_apikey_responses_probe.go 的判定收紧）。
//
// 展示层的人话化（把该文本翻译成人话）在前端 utils/upstreamNotice.ts 完成，
// 两侧的识别正则必须保持同口径。

import (
	"regexp"
	"strings"
)

// upstreamGuardrailNoticePattern 匹配「写给模型看」的护栏提示块。刻意只认有明确
// 包裹形态的两种写法（方括号 System reminder 与 <system-reminder> 标签），不按
// "does not support images" 之类关键词泛化 —— 上游真实报错正文里也会出现这类句子，
// 泛化会把有用的失败原因误判成无信息文本。
var upstreamGuardrailNoticePattern = regexp.MustCompile(`(?is)^\s*(?:\[\s*system[\s_-]*reminder[\s:：]|<\s*system[\s_-]*reminder\s*>).*`)

// upstreamGuardrailPhrasePattern 是护栏提示的语义指纹（必须在包裹形态之内再命中
// 一条「让模型转告用户」的指令句），用于避免把普通的短方括号前缀错误当成护栏。
var upstreamGuardrailPhrasePattern = regexp.MustCompile(`(?i)(content filtered|does not support|switch to a multimodal|alternative approach)`)

// flattenUpstreamSnippet 把上游响应片段压成单行可读文本：换行与连续空白折叠为
// 单个空格，并去除首尾空白。内容本身一字不改。
func flattenUpstreamSnippet(raw string) string {
	return strings.Join(strings.Fields(raw), " ")
}

// isUpstreamGuardrailNotice 报告整段文本是否只是一条上游护栏提示。
// 只判断「以护栏块开头」且「含护栏指令句」的情形：中间夹带真实错误信息的片段返回
// false，交由调用方照常展示，避免丢掉排障线索。
func isUpstreamGuardrailNotice(raw string) bool {
	flattened := flattenUpstreamSnippet(raw)
	if flattened == "" {
		return false
	}
	if !upstreamGuardrailNoticePattern.MatchString(flattened) {
		return false
	}
	return upstreamGuardrailPhrasePattern.MatchString(flattened)
}
