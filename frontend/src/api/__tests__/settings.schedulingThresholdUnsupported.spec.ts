import { describe, expect, it } from "vitest";

import {
  normalizeAccountSchedulingThresholdsMap,
  sanitizeAccountSchedulingThresholdsMap,
  SCHEDULING_THRESHOLD_PERCENT_UNSUPPORTED_PLATFORMS,
  SCHEDULING_THRESHOLD_PLATFORMS,
  supportsSchedulingThresholdPercent,
} from "@/api/admin/settings";

/**
 * 「百分比阈值不可评估的平台」契约防线。
 *
 * 背景：后端 AllowedSchedulingThresholdPlatforms 把 workbuddy / qoder / trae 都列为
 * 可配阈值，但它们的「用量占比」读数不可比（WorkBuddy 的 size 被上游历史累计剂量
 * TotalDosage 抬升、Qoder 的 total 漏算 dedicatedResourcePackages），拿占比停号会误杀
 * 健康号。后端只对 Trae 开放百分比通道，另两平台退回「积分耗尽硬闸门」自动停调。
 *
 * 前端因此把它们的输入框置灰。本文件锁住两件事：
 *  1) 不支持列表与判定函数的对应关系（新增平台时不会漏配）；
 *  2) 置灰**不得**篡改存量数据——normalize 仍按原样保留 1–99 的值（缺失才补 100），
 *     避免「UI 禁用 → 静默把管理员历史配置清零」这种更难排查的数据丢失。
 */
describe("account scheduling threshold percent support", () => {
  it("marks only workbuddy and qoder as percent-unsupported", () => {
    expect(SCHEDULING_THRESHOLD_PERCENT_UNSUPPORTED_PLATFORMS).toEqual(["workbuddy", "qoder"]);
    expect(supportsSchedulingThresholdPercent("workbuddy")).toBe(false);
    expect(supportsSchedulingThresholdPercent("qoder")).toBe(false);
    // Trae 的 used/size 同源于未过期权益包聚合，是唯一可评估百分比的积分平台。
    expect(supportsSchedulingThresholdPercent("trae")).toBe(true);
  });

  it("keeps every other configurable platform percent-capable", () => {
    const supported = SCHEDULING_THRESHOLD_PLATFORMS.filter(
      supportsSchedulingThresholdPercent,
    );
    expect(supported).toEqual(
      SCHEDULING_THRESHOLD_PLATFORMS.filter(
        (platform) => !SCHEDULING_THRESHOLD_PERCENT_UNSUPPORTED_PLATFORMS.includes(platform),
      ),
    );
    // 传统三平台 + 国内套餐类必须仍可配，否则本次改动会误伤既有能力。
    for (const platform of ["openai", "anthropic", "grok", "kimi", "zhipu", "minimax"]) {
      expect(
        supported,
        `${platform} 不应被收进百分比不支持列表`,
      ).toContain(platform as (typeof supported)[number]);
    }
  });

  it("unsupported list stays a subset of the configurable platform list", () => {
    // 若某天把 workbuddy/qoder 从 SCHEDULING_THRESHOLD_PLATFORMS 移出，
    // supportsSchedulingThresholdPercent 的过滤会失去意义，这里提前拦住。
    for (const platform of SCHEDULING_THRESHOLD_PERCENT_UNSUPPORTED_PLATFORMS) {
      expect(SCHEDULING_THRESHOLD_PLATFORMS).toContain(platform);
    }
  });

  it("disabling the input must not drop an existing stored threshold", () => {
    // 置灰只是阻断交互；v-model 里的存量值仍会经 normalize 提交。
    // 这条断言把「不静默清库」钉住：改 normalize 时必须显式决策，不能顺手归 100。
    const stored = normalizeAccountSchedulingThresholdsMap({ workbuddy: 80, qoder: 70 });
    expect(stored.workbuddy).toBe(80);
    expect(stored.qoder).toBe(70);
    expect(sanitizeAccountSchedulingThresholdsMap({ workbuddy: 80 }).workbuddy).toBe(80);
  });

  it("defaults a missing platform to 100 (disabled) rather than an active pause", () => {
    const stored = normalizeAccountSchedulingThresholdsMap({ openai: 85 });
    expect(stored.workbuddy).toBe(100);
    expect(stored.qoder).toBe(100);
    expect(stored.trae).toBe(100);
    expect(stored.openai).toBe(85);
  });
});
