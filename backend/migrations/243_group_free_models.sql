-- 243_group_free_models.sql
-- 分组级「免费模型名单」：命中的模型在本分组按 0 元计费。
--
-- 为什么需要它：网关的「未定价即拒绝」准入闸门（pricing.require_priced_models，默认开）
-- 与模型广场/可用渠道/…/v1/models 的列表过滤共用同一个判定源
-- （service.ModelPricingGate）。上游自带的免费模型（目录里查无此名、或 LiteLLM 未收录的
-- 平台自有型号）四处都解析不出定价，于是同时表现为「客户端 404 + 广场上完全看不到」。
-- 本字段是分组侧的显式定价豁免：名单内的模型按「全 0 价卡」参与定价解析，
-- 因此调得通、算得出 0 元、也上得了广场。
--
-- 优先级（与 service.resolveGroupModelPricing 严格一致）：
--   分组自定义价卡（有实际价格） > 免费名单合成的全 0 卡 > 渠道价 > LiteLLM 目录 > 内置兜底卡
-- 即：管理员为该模型配过真实价卡时以价卡为准，免费名单不会把已配的价格洗成 0；
-- 空价卡（只绑模型名不填价）不足以定案，仍由免费名单兜底。
--
-- 重入安全：ADD COLUMN IF NOT EXISTS + NOT NULL DEFAULT。
ALTER TABLE groups
    ADD COLUMN IF NOT EXISTS free_models JSONB NOT NULL DEFAULT '[]'::jsonb;

COMMENT ON COLUMN groups.free_models IS
    '分组免费模型名单：命中的模型在本分组按 0 元计费（全 0 价卡），可正常调用并出现在模型广场；条目为模型名，支持末尾 * 通配';
