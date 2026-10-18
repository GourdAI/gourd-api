/**
 * 品牌更名（Sub2API -> Gourd API）后，浏览器本地存储键同步改为 `gourd*` 前缀。
 *
 * 只有「用户可见状态」需要迁移：语言偏好、登录协议同意回执。直接换键会让老用户
 * 的语言被重置、被要求重新签署协议，所以这里读到旧键就搬到新键并删除旧键。
 *
 * 纯缓存 / 瞬时锁类（IP 归属地缓存、token 刷新锁、去重锁、批量生图预览 IndexedDB）
 * 不做迁移：丢失后果只是重新查一次，而保留旧键反而留下永远回收不掉的垃圾数据。
 */

/** 语言偏好（i18n） */
export const LOCALE_STORAGE_KEY = 'gourd_locale'
export const LOCALE_STORAGE_KEY_LEGACY = 'sub2api_locale'

/**
 * 登录协议同意回执。
 * LoginView 与 RegisterView 历史上各自硬编码了同一个键名字面量；这里抽出单一来源，
 * 避免以后只改一边导致两个页面的同意状态分叉。
 */
export const LOGIN_AGREEMENT_STORAGE_KEY = 'gourd_login_agreement_consent'
export const LOGIN_AGREEMENT_STORAGE_KEY_LEGACY = 'sub2api_login_agreement_consent'

/**
 * 读取 newKey；缺失时回退 legacyKey，并把值搬到 newKey、删除 legacyKey。
 * 返回 null 表示两处都没有（或存储不可用）。
 */
export function readStoredValueWithMigration(newKey: string, legacyKey: string): string | null {
  const storage = safeLocalStorage()
  if (!storage) return null
  try {
    const current = storage.getItem(newKey)
    if (current !== null) return current

    const legacy = storage.getItem(legacyKey)
    if (legacy === null) return null

    // 搬运而不是复制：迁移完成后旧键必须消失，否则浏览器里永久残留品牌痕迹。
    storage.setItem(newKey, legacy)
    storage.removeItem(legacyKey)
    return legacy
  } catch {
    // 隐私模式 / 配额满：降级为「无记录」，调用方按首次访问处理。
    return null
  }
}

/** 写入 newKey，并顺带清理 legacyKey（历史版本可能已写过旧键）。 */
export function writeStoredValueWithMigration(newKey: string, value: string, legacyKey: string): void {
  const storage = safeLocalStorage()
  if (!storage) return
  try {
    storage.setItem(newKey, value)
    storage.removeItem(legacyKey)
  } catch {
    // 与旧实现一致：原先也是裸 setItem 且不处理异常
  }
}

/** 显式清除：两个键一起清，避免清了新键又被旧键「复活」。 */
export function clearStoredValueWithMigration(newKey: string, legacyKey: string): void {
  const storage = safeLocalStorage()
  if (!storage) return
  try {
    storage.removeItem(newKey)
    storage.removeItem(legacyKey)
  } catch {
    // 同上，静默降级
  }
}

function safeLocalStorage(): Storage | null {
  try {
    return typeof window === 'undefined' ? null : window.localStorage
  } catch {
    // Safari 无痕模式下访问 window.localStorage 本身就会抛错
    return null
  }
}
