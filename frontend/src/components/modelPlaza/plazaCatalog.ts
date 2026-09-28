/**
 * 模型广场目录（纯函数）:把接口返回的「分组 → 模型」展平成「模型 → 所属分组」,
 * 让页面以模型为主视角展示。
 *
 * 展示价口径（用户拍板）:
 * - 选中某个分组 → 卡片展示该分组的实付价;
 * - 「全部」 → 展示当前筛选可见分组中最低的实付价;
 * - 卡片底部列出该模型可用的全部分组及其倍率（含被筛掉的,仅作信息）,
 *   点击某个分组标签即把整页价格切到该分组。
 *
 * 合并粒度:一张卡 = 一个模型名。后端模型去重键是 (platform, name),composite 分组内
 * 允许同名不同平台的条目并存,因此「分组 → 模型」必须按 (分组, 平台) 保留,
 * 否则后写的那份会静默覆盖前者、卡片价格张冠李戴。
 */
import type { ModelPlazaGroup, PlazaModel } from '@/api/modelPlaza'
import {
  isTokenModel,
  modelRateStruck,
  modelRateValue,
  modelSortPrice,
  type PlazaPriceContext
} from './plazaPricing'

/** 卡片底部的单个分组标签（含该组对该模型的价格环境与倍率）。 */
export interface PlazaGroupRef {
  /** 分组内标签的唯一键 = 分组 id + 平台（同组同名多平台时各占一条）。 */
  key: string
  id: number
  name: string
  /** 该条定价实际来源的平台（模型的平台,不是分组的平台——composite 分组平台是聚合值）。 */
  platform: string
  subscriptionType: string
  isExclusive: boolean
  /** 该组对该模型生效的倍率标签（token 取生效倍率,按次/按图取按次或独立图倍率）。 */
  rateValue: number
  /** 需要划线的原倍率（仅用户专属倍率场景）。 */
  struckRate: number | null
  /** 该组下的实付输入单价（$/1M token 或 $/次）,用于跨组比价与排序;无价为 null。 */
  sortPrice: number | null
  /** 是否落在当前分组 / 倍率筛选内;false 时只作信息展示,不参与计价。 */
  visible: boolean
  context: PlazaPriceContext
}

/** 一张卡片 = 一个模型。 */
export interface PlazaModelEntry {
  name: string
  /** 目录主键（同名模型合并后唯一）,渲染 :key 与内部映射都以此为准。 */
  key: string
  /** 该模型出现过的平台（同名跨平台时多个）。 */
  platforms: string[]
  /** 该模型可用的全部分组,按接口返回序。 */
  groups: PlazaGroupRef[]
  /** 当前展示价取自哪个分组。 */
  displayGroupId: number
  model: PlazaModel
  context: PlazaPriceContext
}

/** 分组 → 卡片价格环境。 */
function groupPriceContext(group: ModelPlazaGroup, peakWindow = ''): PlazaPriceContext {
  return {
    rateMultiplier: group.rate_multiplier,
    userRateMultiplier: group.user_rate_multiplier ?? null,
    imageRateIndependent: group.image_rate_independent,
    imageRateMultiplier: group.image_rate_multiplier,
    peakWindow,
    peakRateMultiplier: group.peak_rate_multiplier
  }
}

export interface BuildCatalogOptions {
  /** 通过分组 / 倍率筛选的分组 id;决定哪些分组参与计价,以及模型是否出现在结果里。 */
  visibleGroupIds: Set<number>
  /** 优先展示该分组的价格;'all' 时取可见分组中的最低价。 */
  preferredGroupId?: number | 'all'
  /** 分组 id → 高峰窗口描述（含时区标注）。 */
  peakWindows?: Map<number, string>
}

/** 分组内一条定价的键:分隔符用 NUL,分组 id 与平台名都不会包含它。 */
function refKey(groupId: number, platform: string): string {
  return `${groupId}\u0000${platform}`
}

function toGroupRef(
  group: ModelPlazaGroup,
  model: PlazaModel,
  visibleGroupIds: Set<number>,
  peakWindow: string
): PlazaGroupRef {
  const context = groupPriceContext(group, peakWindow)
  return {
    key: refKey(group.id, model.platform),
    id: group.id,
    // 分组名为空时兜底成 #id,避免 chip 退化成孤立的「0.5x」、tooltip 缺主语
    name: group.name || `#${group.id}`,
    platform: model.platform,
    subscriptionType: group.subscription_type,
    isExclusive: group.is_exclusive,
    rateValue: modelRateValue(model, context),
    struckRate: modelRateStruck(model, context),
    sortPrice: modelSortPrice(model, context),
    visible: visibleGroupIds.has(group.id),
    context
  }
}

/** 可见分组中挑最低实付价;未配价的分组靠后,同价取先出现的分组。 */
function pickCheapest(refs: PlazaGroupRef[]): PlazaGroupRef | undefined {
  let best: PlazaGroupRef | undefined
  for (const ref of refs) {
    if (ref.sortPrice == null) continue
    if (best == null || best.sortPrice == null || ref.sortPrice < best.sortPrice) best = ref
  }
  return best ?? refs.find((ref) => ref.visible)
}

export interface PlazaModelDraft {
  name: string
  platforms: string[]
  groups: PlazaGroupRef[]
  /** (分组, 平台) → 该条原始模型数据（不同分组/平台的定价与分时配置可能不同）。 */
  models: Map<string, PlazaModel>
}

/** 展平成模型目录:同名模型合并成一条卡片,分组标签齐全。 */
export function buildPlazaModelDrafts(
  groups: ModelPlazaGroup[],
  { visibleGroupIds, peakWindows }: Pick<BuildCatalogOptions, 'visibleGroupIds' | 'peakWindows'>
): PlazaModelDraft[] {
  const drafts = new Map<string, PlazaModelDraft>()
  for (const group of groups ?? []) {
    const peakWindow = peakWindows?.get(group.id) ?? ''
    for (const model of group.models ?? []) {
      const ref = toGroupRef(group, model, visibleGroupIds, peakWindow)
      const draft = drafts.get(model.name)
      if (draft) {
        draft.groups.push(ref)
        draft.models.set(ref.key, model)
        if (!draft.platforms.includes(model.platform)) draft.platforms.push(model.platform)
      } else {
        drafts.set(model.name, {
          name: model.name,
          platforms: [model.platform],
          groups: [ref],
          models: new Map([[ref.key, model]])
        })
      }
    }
  }
  return [...drafts.values()]
}

/**
 * 展平为模型维度目录。只保留「至少有一个可见分组」的模型,
 * 保证卡片价格永远落在当前筛选语境内。
 */
export function buildPlazaModelEntries(
  groups: ModelPlazaGroup[],
  { visibleGroupIds, preferredGroupId = 'all', peakWindows }: BuildCatalogOptions
): PlazaModelEntry[] {
  const entries: PlazaModelEntry[] = []
  for (const draft of buildPlazaModelDrafts(groups, { visibleGroupIds, peakWindows })) {
    const visible = draft.groups.filter((g) => g.visible)
    if (!visible.length) continue
    const preferred =
      preferredGroupId !== 'all'
        ? visible.find((g) => g.id === preferredGroupId)
        : undefined
    const display = preferred ?? pickCheapest(visible)
    if (!display) continue
    const model = draft.models.get(display.key)
    if (!model) continue
    entries.push({
      name: draft.name,
      key: draft.name,
      platforms: draft.platforms,
      groups: draft.groups,
      displayGroupId: display.id,
      model,
      context: display.context
    })
  }
  return entries
}

/** 目录内排序键。 */
export type PlazaSortKey = 'default' | 'priceAsc' | 'priceDesc' | 'nameAsc'

/** 当前展示价对应的实付输入单价（用于排序与比价）。 */
export function entrySortPrice(entry: PlazaModelEntry): number | null {
  return modelSortPrice(entry.model, entry.context)
}

/**
 * 名称比较开启 numeric:模型名普遍带版本号,逐字符比较会把 gpt-10 排到 gpt-9 前面。
 * 固定 sensitivity,避免不同浏览器 locale 下中文/混排大小写顺序不一致。
 */
function compareName(a: PlazaModelEntry, b: PlazaModelEntry): number {
  return a.name.localeCompare(b.name, undefined, { numeric: true, sensitivity: 'base' })
}

/**
 * 排序:任何模式下非 token 计费（按次/按图,与 token 价不同量纲）都沉底。
 * - default: 实付输入价降序（高价/前沿型号优先）,同价按名称降序（新版本号在前）;
 * - priceAsc / priceDesc: 实付输入价升/降序,未定价沉底;
 * - nameAsc: 模型名升序。
 */
export function sortPlazaModelEntries(
  entries: PlazaModelEntry[],
  sortKey: PlazaSortKey
): PlazaModelEntry[] {
  const price = new Map(entries.map((e) => [e.key, entrySortPrice(e)]))
  const tokenFirst = (a: PlazaModelEntry, b: PlazaModelEntry): number => {
    const ta = isTokenModel(a.model)
    const tb = isTokenModel(b.model)
    return ta === tb ? 0 : ta ? -1 : 1
  }
  const byPrice = (a: PlazaModelEntry, b: PlazaModelEntry, asc: boolean): number => {
    const pa = price.get(a.key) ?? null
    const pb = price.get(b.key) ?? null
    if (pa == null && pb == null) return 0
    if (pa == null) return 1
    if (pb == null) return -1
    return asc ? pa - pb : pb - pa
  }
  const list = [...entries]
  switch (sortKey) {
    case 'priceAsc':
      return list.sort((a, b) => tokenFirst(a, b) || byPrice(a, b, true) || compareName(a, b))
    case 'priceDesc':
      return list.sort((a, b) => tokenFirst(a, b) || byPrice(a, b, false) || compareName(a, b))
    case 'nameAsc':
      return list.sort((a, b) => tokenFirst(a, b) || compareName(a, b))
    default:
      return list.sort((a, b) => tokenFirst(a, b) || byPrice(a, b, false) || -compareName(a, b))
  }
}

/** 搜索命中:模型名或平台名包含关键词（大小写不敏感）。 */
export function matchPlazaModelEntry(entry: PlazaModelEntry, query: string): boolean {
  const q = query.trim().toLowerCase()
  if (!q) return true
  if (entry.name.toLowerCase().includes(q)) return true
  return entry.platforms.some((p) => p.toLowerCase().includes(q))
}
