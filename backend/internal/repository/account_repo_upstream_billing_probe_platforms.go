package repository

// upstreamBillingProbePlatformsSQL 是 service.IsUpstreamBillingProbeIdentity 的
// SQL 镜像（与 ollamaCloudUsagePlatformsSQL 同一先例：平台白名单 SQL 只允许引用
// 本常量，不得各处重写字面量，防止漂移）。
//
// 候选 SQL 必须按平台收窄，否则「存量 enabled=true 的 WorkBuddy 账号」（该平台凭据
// 为 access/refresh token，无 credentials.api_key，已被移出探测资格名单）会占满
// upstreamBillingProbeMaxPerCycle 个批次名额，饿死同批合格账号。服务层
// isUpstreamBillingProbeAccount 仍会二次过滤，两者互为兜底。
//
// 注意：只应用于「筛选待探测账号」的查询。任何按 id 定位账号的 CAS 更新
// （如 updateProbeSnapshot 的 UPDATE ... WHERE id = $2）不得加平台条件，
// 否则平台收窄后存量账号的快照/失败状态将永远无法写入。
const upstreamBillingProbePlatformsSQL = "'openai', 'anthropic', 'gemini', 'antigravity', 'grok', 'kimi', 'zhipu', 'deepseek', 'minimax', 'opencode_go'"
