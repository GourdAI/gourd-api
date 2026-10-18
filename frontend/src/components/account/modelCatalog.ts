import { getModelAllowlistCandidates } from '@/api/admin/groups'
import type { GroupPlatform } from '@/types'
import { DYNAMIC_CATALOG_PLATFORMS } from '@/composables/useModelWhitelist'

/**
 * modelCatalog.ts —— 固定协议平台（WorkBuddy / Trae / Qoder）的模型目录动态来源。
 *
 * 为什么要把它单独抽出来：这三家的模型表原先写死在前端常量里，上游上新后前端
 * 永远看不到 —— 管理员只能「先建号 → 再进编辑页点同步」，白做一遍。现在常量已删，
 * 目录的唯一来源是后端权威默认表（GET /admin/groups/0/model-allowlist-candidates）。
 *
 * 这里直接调 groups 模块的具名导出（而不是 adminAPI 聚合对象）：单测普遍只 mock
 * '@/api/admin/groups'，走聚合入口会把未被 mock 的其它 API 模块一起拽进来。
 *
 * 缓存按目标一份（模块级）：新建弹窗打开、切平台、下拉展开都命中同一份数据，
 * 不会每敲一次键盘就打一次后端。失败不写缓存，下次调用自然重试。
 */

const cache = new Map<string, string[]>()
const inflight = new Map<string, Promise<string[]>>()

export function isDynamicCatalogPlatform(platform?: string | null): boolean {
  return !!platform && DYNAMIC_CATALOG_PLATFORMS.has(platform.trim().toLowerCase())
}

/**
 * 拉取一个目录目标的模型 ID 列表。target 可以是具体平台（workbuddy/qoder/trae），
 * 也可以是 'composite'（后端返回全平台并集，用于没传 platform 的选择器）。
 *
 * force=true 跳过缓存：本缓存没有 TTL（一次会话内命中就再也不打后端），而
 * 「同步最新支持模型」的语义就是主动刷新，不绕过它这个按钮永远只会拿到首次结果。
 */
export async function fetchCatalogModels(target: string, force = false): Promise<string[]> {
  const key = target.trim().toLowerCase()
  if (!key) return []
  if (!force) {
    const cached = cache.get(key)
    if (cached) return cached
  }

  const pending = inflight.get(key)
  if (!force && pending) return pending

  const request = getModelAllowlistCandidates(0, key as GroupPlatform)
    .then(models => {
      const list = Array.from(new Set((models || []).map(id => id.trim()).filter(Boolean)))
      // 空结果不写缓存：一次上游抖动不该把该平台永久钉成「没有模型」。
      if (list.length > 0) cache.set(key, list)
      return list
    })
    .finally(() => {
      inflight.delete(key)
    })

  inflight.set(key, request)
  return request
}

/**
 * 平台默认模型目录（后端权威表）。非动态平台返回空数组。
 *
 * 这里**不能**自己吞错：调用方（新建弹窗预填）需要区分「接口失败」与
 * 「真的一个模型都没有」，失败时它要保持空白名单（=不限制模型）而不是
 * 把空数组当成正确结果写进去。错误因此原样冒泡给调用方的 .catch。
 */
export async function fetchDefaultModelCatalog(platform: string, force = false): Promise<string[]> {
  if (!isDynamicCatalogPlatform(platform)) return []
  return fetchCatalogModels(platform, force)
}

/** 测试用：清空模块级缓存，避免用例之间互相污染。 */
export function resetModelCatalogCache(): void {
  cache.clear()
  inflight.clear()
}
