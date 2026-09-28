import { describe, expect, it } from 'vitest'
import { describeUpstreamGuardrailNotice } from '../upstreamNotice'

const NOTICE =
  '[System reminder: the current model does not support images. Content filtered. Please inform user to switch to a multimodal model or try alternative approach.]'
const LABEL = '上游返回了一段护栏提示'

describe('describeUpstreamGuardrailNotice', () => {
  it('replaces a bare guardrail notice with the localized label', () => {
    expect(describeUpstreamGuardrailNotice(NOTICE, LABEL)).toBe(LABEL)
  })

  it('keeps surrounding actionable context', () => {
    const message = `Trae model list returned HTTP 400: ${NOTICE}`
    const result = describeUpstreamGuardrailNotice(message, LABEL)
    expect(result).toContain('Trae model list returned HTTP 400:')
    expect(result).toContain(LABEL)
    expect(result).not.toContain('Content filtered')
  })

  it('handles the tagged form and multi-line payloads', () => {
    const tagged = '<system-reminder>\nthe model does not support images.\nplease switch to a multimodal model\n</system-reminder>'
    const result = describeUpstreamGuardrailNotice(`failed: ${tagged}`, LABEL)
    expect(result).toBe(`failed: ${LABEL}`)
  })

  it('leaves unrelated messages untouched', () => {
    const cases = [
      '同步上游模型失败：连接超时',
      '{"error":{"message":"model does not support images"}}',
      '[not-found] model gpt-x is unavailable',
      '<system-reminder>Today is 2026-09-27.</system-reminder>',
      '每行一个 key\n支持多个'
    ]
    for (const message of cases) {
      expect(describeUpstreamGuardrailNotice(message, LABEL)).toBe(message)
    }
  })

  it('short-circuits empty input', () => {
    expect(describeUpstreamGuardrailNotice('', LABEL)).toBe('')
  })

  it('collapses repeated notices into one label', () => {
    const result = describeUpstreamGuardrailNotice(`${NOTICE}\n${NOTICE}`, LABEL)
    expect(result).toBe(LABEL)
  })
})
