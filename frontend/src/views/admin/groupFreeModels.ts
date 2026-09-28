// 分组级「免费模型名单」（free_models）纯逻辑模块。
//
// 语义与模型白名单不同：这里没有 enabled 开关，数组非空即生效，空数组即关闭。
// 命中的模型在该分组按 0 元计费（仍可正常调用，并出现在模型广场）。
// 条目为模型名，允许末尾 * 通配。
//
// 实现范式严格对齐 ./groupModelAllowlist.ts，并复用其导出的条目类型与错误码。
import type {
  ModelAllowlistAddError,
  ModelAllowlistItem,
} from "./groupModelAllowlist";

export type { ModelAllowlistAddError, ModelAllowlistItem };

// 与白名单共用同一套错误码，由视图映射为 i18n 提示。
export type FreeModelsAddError = ModelAllowlistAddError;

export interface FreeModelState {
  savedModels: string[]
  items: ModelAllowlistItem[]
}

export const createFreeModelState = (
  config?: string[] | null,
): FreeModelState => ({
  savedModels: normalizeModels(config ?? []),
  items: [],
})

export const hydrateFreeModelState = (
  config: string[] | null | undefined,
  candidates: string[],
): FreeModelState => {
  const state = createFreeModelState(config)
  setFreeModelCandidates(state, candidates)
  return state
}

// setFreeModelCandidates 把候选模型灌成条目列表：
// 已保存的条目置顶并保持选中，其余（候选 / 手工条目）按当前勾选状态保留。
// 与白名单版的差别：没有 savedModels 时不会默认全选候选（否则等于全部分组免费）。
export const setFreeModelCandidates = (
  state: FreeModelState,
  candidates: string[],
) => {
  const normalizedCandidates = normalizeModels(candidates)
  const currentSelected = new Set(
    state.items.filter(item => item.selected).map(item => item.id),
  )
  const savedSelected = new Set(state.savedModels)
  // 已有条目集合：用于区分「用户显式取消勾选」与「候选还未入列」。
  const existingIDs = new Set(state.items.map(item => item.id))
  const hasExistingItems = state.items.length > 0
  const selectionOrder = normalizeModels([
    ...state.items.map(item => item.id),
    ...state.savedModels,
    ...normalizedCandidates,
  ])

  state.items = selectionOrder.map(id => ({
    id,
    // 候选迟到竞态：手工输入框在候选未到时就能用，用户先加了自定义条目后
    // state.items 已非空；此时若只看 currentSelected，已保存的 savedModels 会被
    // 算成未勾选，提交后后端全列覆盖直接丢掉它们——而免费名单少一条的代价是
    // 该模型从免费变收费，比白名单少一条更难察觉。因此：已保存且尚未入列的条目
    // 默认保持选中；已在列表里且被取消的（existingIDs 命中）则尊重用户。
    selected: hasExistingItems
      ? currentSelected.has(id) ||
        (savedSelected.has(id) && !existingIDs.has(id))
      : savedSelected.has(id),
  }))
}

export const toggleFreeModelItem = (
  state: FreeModelState,
  modelID: string,
) => {
  const item = state.items.find(item => item.id === modelID)
  if (item) {
    item.selected = !item.selected
  }
}

export const selectAllFreeModelItems = (state: FreeModelState) => {
  state.items.forEach(item => {
    item.selected = true
  })
}

export const invertFreeModelSelection = (state: FreeModelState) => {
  state.items.forEach(item => {
    item.selected = !item.selected
  })
}

export const moveFreeModelItem = (
  state: FreeModelState,
  fromIndex: number,
  toIndex: number,
) => {
  if (
    fromIndex === toIndex ||
    fromIndex < 0 ||
    toIndex < 0 ||
    fromIndex >= state.items.length ||
    toIndex >= state.items.length
  ) {
    return
  }
  const [item] = state.items.splice(fromIndex, 1)
  state.items.splice(toIndex, 0, item)
}

// addCustomFreeModelItem 把手工输入的条目追加到名单末尾（选中状态）。
// 去重；`*` 只允许出现在末尾。返回错误码或 null（成功）。
export const addCustomFreeModelItem = (
  state: FreeModelState,
  raw: string,
): FreeModelsAddError | null => {
  const entry = raw.trim()
  if (!entry) {
    return 'empty'
  }
  if (entry.slice(0, -1).includes('*')) {
    return 'invalid_wildcard'
  }
  if (
    state.items.some(item => item.id.toLowerCase() === entry.toLowerCase()) ||
    state.savedModels.some(model => model.toLowerCase() === entry.toLowerCase())
  ) {
    return 'duplicate'
  }
  state.items.push({ id: entry, selected: true })
  return null
}

// buildFreeModelsPayload 返回提交的数组：一条名单都未勾选时回落已保存条目，
// 空数组即代表关闭该功能（与白名单不同，这里不做必填校验）。
export const buildFreeModelsPayload = (state: FreeModelState): string[] =>
  state.items.length > 0
    ? state.items.filter(item => item.selected).map(item => item.id)
    : [...state.savedModels]

export const selectedFreeModelCount = (state: FreeModelState): number =>
  state.items.filter(item => item.selected).length

// normalizeModels 去空白 + 去重。**去重口径与后端 normalizeGroupFreeModels 一致**：
// 按小写判重、保留首次出现的原形。否则同一模型以 `GPT-5.5` 与 `gpt-5.5` 两行入选，
// UI 显示「2 已选」而后端折叠成 1 条，计数与真相不符。
const normalizeModels = (models: string[]): string[] => {
  const seen = new Set<string>()
  const out: string[] = []
  for (const raw of models) {
    const model = raw.trim()
    const dedupeKey = model.toLowerCase()
    if (!model || seen.has(dedupeKey)) {
      continue
    }
    seen.add(dedupeKey)
    out.push(model)
  }
  return out
}
