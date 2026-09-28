/**
 * 上游「写给模型看」护栏提示的展示层收口。
 *
 * 部分上游（自研网关、聚合站）在能力不匹配时不返回结构化 error，而是把一段写给
 * 模型看的指令文本当作正常内容回吐，例如：
 *   [System reminder: the current model does not support images. Content filtered.
 *    Please inform user to switch to a multimodal model or try alternative approach.]
 *
 * 这类文本会随上游响应体片段进入管理端提示（同步上游模型失败原因、账号状态等），
 * 管理员看到既读不懂也无从行动。这里把它替换成一句人话，原文其余部分保持不变。
 *
 * 与后端 internal/service/upstream_notice.go 保持同一识别口径：必须是「明确包裹
 * 形态」+「护栏指令句」双条件命中，避免把含 similar 字样的真实报错一并吞掉。
 */

const NOTICE_PHRASE = /content filtered|does not support|switch to a multimodal|alternative approach/i

// 先粗筛：没出现 system reminder 字样的文案（绝大多数正常提示）直接短路，
// 不进入下面的全局替换扫描。
const GUARDRAIL_HEAD_PATTERN = /system[\s_-]*reminder/i

const NOTICE_PATTERNS: RegExp[] = [
  /\[\s*system[\s_-]*reminder[\s:：][^\]]*\]/gi,
  /<\s*system[\s_-]*reminder\s*>[\s\S]*?<\s*\/\s*system[\s_-]*reminder\s*>/gi
]

/**
 * 廉价形态判定：文案里是否可能出现上游护栏提示。供调用方在真正取多语文案前短路。
 */
export function hasUpstreamGuardrailNoticeHead(message: string): boolean {
  return !!message && GUARDRAIL_HEAD_PATTERN.test(message)
}

/**
 * 把消息里的上游护栏提示替换为 label，并压平多余空白。
 * 未命中时原样返回（不改动任何正常文案，包括含换行的多行提示）。
 */
export function describeUpstreamGuardrailNotice(message: string, label: string): string {
  if (!hasUpstreamGuardrailNoticeHead(message)) return message
  let replaced = message
  for (const pattern of NOTICE_PATTERNS) {
    replaced = replaced.replace(pattern, (matched) =>
      NOTICE_PHRASE.test(matched) ? ` ${label} ` : matched
    )
  }
  if (replaced === message) return message
  // 上游会把同一段提示重复贴进多个字段；先合并相邻重复 label 再压空白。
  replaced = replaced.replace(new RegExp(`${escapeRegExp(label)}(\\s+)?(?=${escapeRegExp(label)})`, 'g'), '')
  return replaced.replace(/\s+/g, ' ').trim()
}

function escapeRegExp(value: string): string {
  return value.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')
}
