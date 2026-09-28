import { describe, expect, it } from "vitest";

import {
  addCustomFreeModelItem,
  buildFreeModelsPayload,
  createFreeModelState,
  hydrateFreeModelState,
  invertFreeModelSelection,
  moveFreeModelItem,
  selectAllFreeModelItems,
  setFreeModelCandidates,
  selectedFreeModelCount,
  toggleFreeModelItem,
  type FreeModelsAddError,
} from "../groupFreeModels";

describe("groupFreeModels", () => {
  it("creates an empty disabled state and never defaults candidates to selected", () => {
    const state = createFreeModelState();

    setFreeModelCandidates(state, ["gpt-5.5", "gpt-5.4"]);

    // 免费名单不是开关：没有已保存条目时候选默认全部不勾选（否则等于全分组免费）。
    expect(state.savedModels).toEqual([]);
    expect(state.items).toEqual([
      { id: "gpt-5.5", selected: false },
      { id: "gpt-5.4", selected: false },
    ]);
    expect(buildFreeModelsPayload(state)).toEqual([]);
  });

  it("normalizes saved entries with trim, dedupe and order preserved", () => {
    const state = createFreeModelState([
      "  gpt-5.5  ",
      "gpt-5.5",
      "",
      "claude-*",
    ]);

    expect(state.savedModels).toEqual(["gpt-5.5", "claude-*"]);
  });

  it("keeps saved entries on top selected and new candidates unselected", () => {
    const state = createFreeModelState(["gpt-5.5", "gpt-5.4"]);

    setFreeModelCandidates(state, ["legacy-gpt", "gpt-5.4", "gpt-5.5"]);

    expect(state.items).toEqual([
      { id: "gpt-5.5", selected: true },
      { id: "gpt-5.4", selected: true },
      { id: "legacy-gpt", selected: false },
    ]);
  });

  it("preserves explicit selection when candidates refresh", () => {
    const state = hydrateFreeModelState(["gpt-5.5"], ["gpt-5.5", "gpt-5.4"]);

    toggleFreeModelItem(state, "gpt-5.5");
    toggleFreeModelItem(state, "gpt-5.4");
    setFreeModelCandidates(state, ["gpt-5.5", "gpt-5.4", "gpt-5.4-mini"]);

    expect(state.items).toEqual([
      { id: "gpt-5.5", selected: false },
      { id: "gpt-5.4", selected: true },
      { id: "gpt-5.4-mini", selected: false },
    ]);
  });

  it("keeps saved entries selected when candidates arrive after a custom entry", () => {
    // 回归：候选接口还在飞行时手工加了一条，此时 state.items 已非空；
    // 旧实现会走到 `currentSelected.has(id)` 分支，把已保存条目算成未勾选，
    // 提交后后端全列覆盖直接丢掉它们——免费名单少一条等于该模型开始收费。
    const state = createFreeModelState(["gpt-5.5", "gpt-5.4"]);

    addCustomFreeModelItem(state, "manual-free-model");
    setFreeModelCandidates(state, ["gpt-5.5", "gpt-5.4", "gpt-5.4-mini"]);

    expect(buildFreeModelsPayload(state)).toEqual([
      "manual-free-model",
      "gpt-5.5",
      "gpt-5.4",
    ]);
  });

  it("still honours an explicit unselect of a saved entry after candidates arrive", () => {
    const state = createFreeModelState(["gpt-5.5"]);

    // 先灌候选（条目入列），再取消勾选，最后候选刷新——不能把用户意图抹掉。
    setFreeModelCandidates(state, ["gpt-5.5", "gpt-5.4"]);
    toggleFreeModelItem(state, "gpt-5.5");
    addCustomFreeModelItem(state, "manual-free-model");
    setFreeModelCandidates(state, ["gpt-5.5", "gpt-5.4", "gpt-5.4-mini"]);

    expect(buildFreeModelsPayload(state)).toEqual(["manual-free-model"]);
  });

  it("dedupes saved and candidate entries case-insensitively like the backend", () => {
    const state = createFreeModelState(["GPT-5.5"]);

    setFreeModelCandidates(state, ["gpt-5.5", "gpt-5.5"]);

    // 后端按小写去重；前端若大小写敏感会渲染两行并显示「2 已选」，与保存结果不符。
    expect(state.items).toEqual([{ id: "GPT-5.5", selected: true }]);
    expect(selectedFreeModelCount(state)).toBe(1);
  });

  it("builds payload from selected items in display order", () => {
    const state = hydrateFreeModelState(
      ["gpt-5.5", "gpt-5.4", "legacy-gpt"],
      ["gpt-5.5", "gpt-5.4", "legacy-gpt"],
    );

    toggleFreeModelItem(state, "legacy-gpt");
    moveFreeModelItem(state, 1, 0);

    expect(buildFreeModelsPayload(state)).toEqual(["gpt-5.4", "gpt-5.5"]);
  });

  it("falls back to saved models while candidates are still loading", () => {
    const state = createFreeModelState(["gpt-5.5", "gpt-5.4"]);

    expect(buildFreeModelsPayload(state)).toEqual(["gpt-5.5", "gpt-5.4"]);
  });

  it("returns an empty array when everything is unselected so the feature turns off", () => {
    const state = hydrateFreeModelState(["gpt-5.5"], ["gpt-5.5", "gpt-5.4"]);

    toggleFreeModelItem(state, "gpt-5.5");

    expect(state.items).toEqual([
      { id: "gpt-5.5", selected: false },
      { id: "gpt-5.4", selected: false },
    ]);
    expect(buildFreeModelsPayload(state)).toEqual([]);
    expect(selectedFreeModelCount(state)).toBe(0);
  });

  it("selects all candidates from the toolbar action", () => {
    const state = hydrateFreeModelState(["gpt-5.5"], ["gpt-5.5", "gpt-5.4"]);

    selectAllFreeModelItems(state);

    expect(buildFreeModelsPayload(state)).toEqual(["gpt-5.5", "gpt-5.4"]);
    expect(selectedFreeModelCount(state)).toBe(2);
  });

  it("inverts the current selection from the toolbar action", () => {
    const state = hydrateFreeModelState(["gpt-5.5"], ["gpt-5.5", "gpt-5.4"]);

    invertFreeModelSelection(state);

    expect(state.items).toEqual([
      { id: "gpt-5.5", selected: false },
      { id: "gpt-5.4", selected: true },
    ]);
    expect(buildFreeModelsPayload(state)).toEqual(["gpt-5.4"]);
  });

  it("ignores out-of-range moves", () => {
    const state = hydrateFreeModelState(["gpt-5.5"], ["gpt-5.5", "gpt-5.4"]);

    moveFreeModelItem(state, 5, 1);
    moveFreeModelItem(state, -1, 0);
    moveFreeModelItem(state, 0, 0);

    expect(state.items.map(item => item.id)).toEqual(["gpt-5.5", "gpt-5.4"]);
  });

  it("keeps toggling of unknown ids harmless", () => {
    const state = hydrateFreeModelState(["gpt-5.5"], ["gpt-5.5"]);

    toggleFreeModelItem(state, "nope");

    expect(state.items).toEqual([{ id: "gpt-5.5", selected: true }]);
  });
});

describe("addCustomFreeModelItem", () => {
  const state = () => createFreeModelState(["gpt-5.4"]);

  it("appends a trimmed entry as selected", () => {
    const s = state();
    expect(addCustomFreeModelItem(s, "  claude-sonnet-4.5  ")).toBeNull();
    expect(s.items.at(-1)).toEqual({ id: "claude-sonnet-4.5", selected: true });
    expect(buildFreeModelsPayload(s)).toEqual(["claude-sonnet-4.5"]);
  });

  it("accepts trailing wildcard entries", () => {
    const s = state();
    expect(addCustomFreeModelItem(s, "gpt-5.5-*")).toBeNull();
    expect(s.items.at(-1)?.id).toBe("gpt-5.5-*");
  });

  it("rejects blank input", () => {
    expect(addCustomFreeModelItem(state(), "   ")).toBe<FreeModelsAddError>("empty");
  });

  it("rejects wildcards that are not trailing", () => {
    expect(addCustomFreeModelItem(state(), "gpt-*-5.4")).toBe<FreeModelsAddError>("invalid_wildcard");
    expect(addCustomFreeModelItem(state(), "gpt-*-codex-*")).toBe<FreeModelsAddError>("invalid_wildcard");
  });

  it("rejects duplicates case-insensitively against saved models and items", () => {
    const s = state();
    expect(addCustomFreeModelItem(s, "GPT-5.4")).toBe<FreeModelsAddError>("duplicate");
    addCustomFreeModelItem(s, "claude-*");
    expect(addCustomFreeModelItem(s, "CLAUDE-*")).toBe<FreeModelsAddError>("duplicate");
  });
});
