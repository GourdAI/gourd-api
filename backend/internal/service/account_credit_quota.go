package service

// account_credit_quota.go —— Trae / Qoder / WorkBuddy 积分快照的调度语义层。
//
// 【本文件解决的问题】
// 这三个平台的积分快照（account.Extra 的 trae_credits / qoder_credits /
// workbuddy_credits）此前**只被管理面板读取**，调度链上没有任何一处消费它：
//   - Account.IsSchedulable 只看 apikey/bedrock 的 USD 配额（account.go:198）；
//   - AllowedSchedulingThresholdPlatforms 把这三平台列为「可配阈值」，但阈值
//     评估器把它们路由到 cnProviderThresholdCandidates，而它读的
//     <provider>_5h_used_percent 从无写入者（CNProviderQuotaService 白名单只有
//     kimi/zhipu/minimax/opencode_go）→ 管理员设的阈值 100% 静默无效。
// 后果：一个积分已经耗尽的号照样被选中、照样打到上游，直到撞出流内错误帧
// （Trae 1005 / Qoder 110-119）才被事后冷却；而 trae_upstream_error.go 文件头
// 记录的旧症状「这一整次请求被记为成功并按 0 token 出账」正是这段空窗期的产物。
//
// 【本轮落地的两条路径，并不对称】
//  1. 硬耗尽闸门（shouldAutoPauseAccountByCredits）——三平台全部接入，但可信证据
//     按平台分别定义（见 creditSnapshotExhausted）。其中只有 Trae 能仅凭本地
//     remain/size 定论；Qoder 只信上游 isQuotaExceeded；WorkBuddy 需 packs+size+used
//     同时成立。
//  2. 百分比阈值（creditSnapshotThresholdCandidate）——**只对 Trae 生效**，因为
//     它是唯一 used/size 同源的 platform；另两平台的占比读数失真（详见该函数）。
//     这一条只修了「管理员配了也无效」的一半，而不是「三平台阈值全部可用」。
//
// 【口径：宁可漏判，不可误杀 —— 且必须按平台分别定义「可信证据」】
// 快照是**拉取式观测数据**，且上游探测失败时不覆盖旧值（persistTraeCreditsSnapshot
// 早退于 !result.Success），所以「0」有四种来源：真耗尽 / 权益包全过期 / 从未探测
// （键不存在）/ 探测降级返回空结构。
//
// 关键前提（本文件最容易犯错的地方）：三个平台的 remain/size/used **聚合规则互不
// 相同**，不能共用一条 `remain<=0 && (size>0 || packs>0)`。逐平台的口径事实：
//  - Trae：traeAggregatePacks 对**未过期**包同时求和 remain/used/size ⇒ 三者同池，
//    比值可信；packs 含过期包 ⇒「packs>0 但 size=0」= 包全过期，是真耗尽。
//  - Qoder：applyQoderQuota 只把 userQuota/addOnQuota 累加进 remaining/total，
//    dedicatedResourcePackages 循环**只 Packs++、不累加余额** ⇒ remain=0 不代表
//    账号没钱（专属包可能还有余额），且 isQuotaExceeded 才是综合所有池的权威读数。
//  - WorkBuddy：remain 是逐包容量字段求和（可信），但 size 会被 TotalDosage 抬升、
//    used 再由 `size-remain` 反推（workbuddyAggregateResource:812-822）⇒ used/size
//    比值虚高（TotalDosage 是历史累计剂量，不是当前额度池），不可用于占比判定。
// 由此定下的判定口径见 creditSnapshotExhausted 与 creditSnapshotThresholdCandidate。
//
// 【新鲜度闸门】
// fetched_at 超过 creditSnapshotFreshWindow 的快照不再参与判定。这一条不是保守，
// 而是**必须的自愈保证**：账号一旦被本层停调就不再收流量 ⇒ 没有任何路径再刷新
// 积分快照（Trae/Qoder 靠手动探测与签到任务），若不设过期，一次误判会把账号
// 永久锁死。窗口取 24h：与套餐按自然日/自然月重置的周期对齐，且停调本身也只
// 持续到次日 0 点（见 creditSnapshotRecoveryUntil），不会出现「停调先到期、
// 快照仍陈旧」的空转。

import (
	"encoding/json"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
)

// creditSnapshotFreshWindow 积分快照的可信年龄上限。
const creditSnapshotFreshWindow = 24 * time.Hour

// creditSnapshotQuotaWindow 停调原因里标注的窗口名（与 codex_5h/7d、grok 的
// window 字段同一命名空间，供 filterStats 与 Ops 事件对齐）。
const creditSnapshotQuotaWindow = "credits"

// accountCreditSnapshot 三个平台积分快照的统一视图。
type accountCreditSnapshot struct {
	platform string

	Remain float64
	Used   float64
	// Size 额度池总量（Qoder 侧对应 total）。
	Size float64
	// PackSizeSum 仅 WorkBuddy：各套餐**未经 TotalDosage 抬升**的容量之和。
	// 它是「上游真读到了额度池容量」的唯一可信正向证据（降级读数下为 0）。
	PackSizeSum float64
	// Credits Trae status 接口口径的总积分；与 Size/Remain 独立，用于区分
	// 「没有权益包但有积分余额」这种不该停调的形态。
	Credits float64

	// Packs 快照里的权益包**总数（含已过期）**——Trae 侧 result.Packs = len(packs)
	// 发生在按 expired 过滤之前（trae_credits_service.go:334），因此不能用它判
	// 「还有可用包」；「是否还有可用额度」一律看 Size（= traeAggregatePacks 跳过
	// 过期包后的总额，天然就是「未过期额度池」）。本字段只用于区分「确实查到过
	// 包但全过期」与「压根没有包」两种零额度形态。
	Packs int

	// ExhaustedSignal 上游显式给出的耗尽布尔（Qoder isQuotaExceeded）。
	ExhaustedSignal bool
	// NotApplicable 上游明确该资源池无可查额度（WorkBuddy 企业成员：额度由
	// 企业后台管理）。此时 remain=0 是「查不到」而不是「用完了」，绝不可停调。
	NotApplicable bool

	FetchedAt int64
}

// readAccountCreditSnapshot 解析账号 extra 上的积分快照。
// 返回 (nil, false) 表示该平台无积分快照语义，或快照键不存在（从未探测）。
//
// 热路径考量：本函数在候选过滤阶段每账号调一次（一个分组可能几百个号），因此
// 不走 json.Marshal/Unmarshal 收敛，而是直接按各平台快照的 JSON 字段名取值。
func readAccountCreditSnapshot(account *Account) (*accountCreditSnapshot, bool) {
	if account == nil || len(account.Extra) == 0 {
		return nil, false
	}
	var key string
	switch account.Platform {
	case PlatformTrae:
		key = traeCreditsExtraKey
	case PlatformQoder:
		key = qoderCreditsExtraKey
	case PlatformWorkbuddy:
		key = workbuddyCreditsExtraKey
	default:
		return nil, false
	}
	raw, ok := account.Extra[key]
	if !ok || raw == nil {
		return nil, false
	}
	fields, ok := creditSnapshotFields(raw)
	if !ok {
		return nil, false
	}
	snapshot := &accountCreditSnapshot{
		platform:  account.Platform,
		Remain:    fields.float("remain"),
		Used:      fields.float("used"),
		Packs:     int(fields.int("packs")),
		FetchedAt: fields.int("fetched_at"),
	}
	switch account.Platform {
	case PlatformTrae:
		snapshot.Size = fields.float("size")
		snapshot.Credits = fields.float("credits")
	case PlatformQoder:
		snapshot.Size = fields.float("total")
		snapshot.ExhaustedSignal = fields.bool("quota_exceeded")
	case PlatformWorkbuddy:
		snapshot.Size = fields.float("size")
		snapshot.PackSizeSum = fields.float("pack_size_sum")
		// 企业成员账号在个人 billing 资源池本就无可查额度（上游 code=0 但
		// Accounts=null），remain=0 是「查不到」而不是「用完了」。
		snapshot.NotApplicable = fields.bool("not_applicable") || fields.bool("enterprise")
	}
	return snapshot, true
}

// creditSnapshotFields 把 extra 上的快照归一为「按 JSON 字段名取值」的视图。
// 内存里可能是具体结构体（本进程刚写入）也可能是 map[string]any（经 DB JSONB
// 或 Redis 投影回读），两种都要支持，否则同一个判定会随调用路径分叉。
func creditSnapshotFields(raw any) (creditSnapshotFieldMap, bool) {
	switch value := raw.(type) {
	case map[string]any:
		return creditSnapshotFieldMap(value), true
	case TraeCreditsSnapshot:
		return structToCreditSnapshotMap(value), true
	case *TraeCreditsSnapshot:
		if value == nil {
			return nil, false
		}
		return structToCreditSnapshotMap(*value), true
	case QoderCreditsSnapshot:
		return structToCreditSnapshotMap(value), true
	case *QoderCreditsSnapshot:
		if value == nil {
			return nil, false
		}
		return structToCreditSnapshotMap(*value), true
	case WorkBuddyCreditsSnapshot:
		return structToCreditSnapshotMap(value), true
	case *WorkBuddyCreditsSnapshot:
		if value == nil {
			return nil, false
		}
		return structToCreditSnapshotMap(*value), true
	default:
		return nil, false
	}
}

// structToCreditSnapshotMap 将快照结构体转为按 json tag 取键的 map。
//
// 成本：一次 json.Marshal **加** 一次 json.Unmarshal（不是只 marshal）。
// 生产路径上通常不执行：账号从 DB JSONB / Redis meta 投影回时 extra 已是
// map[string]any，走 creditSnapshotFields 的 map 分支；只有调用方直接持有
// 结构体（单测、或本进程内探测后尚未过序列化边界的对象）才进到这里。
// 失败（不可序列化/非对象）视为不可用，返回 nil 使调用侧归一为「放行」。
func structToCreditSnapshotMap(value any) creditSnapshotFieldMap {
	data, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		return nil
	}
	return creditSnapshotFieldMap(out)
}

// creditSnapshotFieldMap 从 map 里安全取数（缺键/类型不符一律返回零值）。
type creditSnapshotFieldMap map[string]any

func (m creditSnapshotFieldMap) float(name string) float64 {
	if m == nil {
		return 0
	}
	switch v := m[name].(type) {
	case float64:
		return v
	case float32:
		return float64(v)
	case int:
		return float64(v)
	case int64:
		return float64(v)
	case json.Number:
		parsed, err := v.Float64()
		if err != nil {
			return 0
		}
		return parsed
	default:
		return 0
	}
}

func (m creditSnapshotFieldMap) int(name string) int64 {
	return int64(m.float(name))
}

func (m creditSnapshotFieldMap) bool(name string) bool {
	if m == nil {
		return false
	}
	value, ok := m[name].(bool)
	return ok && value
}

// creditSnapshotIsFresh 报告快照年龄是否落在可信窗口内。
// FetchedAt<=0 的快照（历史数据/上游未回传时间戳）视为不可信：本层的全部安全性
// 都建立在「读数够新」之上，缺了时间戳就无从判断，宁可不拦。
func creditSnapshotIsFresh(snapshot *accountCreditSnapshot, now time.Time) bool {
	if snapshot == nil || snapshot.FetchedAt <= 0 {
		return false
	}
	fetchedAt := time.Unix(snapshot.FetchedAt, 0)
	if now.Before(fetchedAt) {
		return true
	}
	return now.Sub(fetchedAt) < creditSnapshotFreshWindow
}

// creditSnapshotExhausted 报告快照是否给出可信的「额度耗尽」结论。
//
// 落盘契约前提：三个平台的 persist*CreditsSnapshot 都在 result.Success==false 时
// 早退，因此 extra 上的快照**只来自成功探测**，不会出现「上游报错→写个全零快照」
// 这种最危险的误杀源。
//
// 【为什么必须按平台分支】曾经用过一版平台中立的
// `remain<=0 && (size>0 || packs>0)`，自查被两处反例推翻（都是把健康号整体打下线，
// 比原来「耗尽号照样接单」更严重，account_credit_quota_test.go 里有对应回归用例）：
//   - Qoder 有 3 个专属资源包（各剩 100）、套餐池用光 → remain=0/total=0/packs=3
//     → 旧口径判耗尽，实际账号还有 300 余额可用。
//   - WorkBuddy 降级读数（Accounts 非空但容量字段全缺省）→ packs>0 成立、
//     remain=0 是「查不到」而非「用完了」。
func creditSnapshotExhausted(snapshot *accountCreditSnapshot) bool {
	if snapshot == nil || snapshot.NotApplicable {
		return false
	}
	// 上游显式布尔优先：Qoder 的 isQuotaExceeded 是权威读数，它综合了套餐、加油包
	// 与专属资源包（以及本层看不到的并发窗口维度），比本地算出的 remain 更可信，
	// 也是 Qoder 唯一可采信的信号（见下方 PlatformQoder 分支）。
	if snapshot.ExhaustedSignal {
		return true
	}
	// 任一口径仍显示有余额就放行。这一条同时兜住两种「不该停调」的形态：
	// 只剩积分余额（无权益包）与只剩权益包（积分为 0）。
	if snapshot.Remain > 0 || snapshot.Credits > 0 {
		return false
	}
	switch snapshot.platform {
	case PlatformTrae:
		// remain/size 同为「未过期权益包」的聚合（traeAggregatePacks 跳过过期包），
		// 因此 size>0 就是「额度池存在但已用光」的直接证据；packs>0 覆盖「包全部
		// 过期 ⇒ size 归零」这一 Trae 的典型耗尽形态。
		// 两者全为 0 时无正向依据（可能是从未开通套餐的空账号，也可能是权益包明细
		// 接口抖动留下的空读数）→ 放行，交给上游流内错误码 1005 做事后处置。
		return snapshot.Size > 0 || snapshot.Packs > 0
	case PlatformQoder:
		// 不可用本地 remain/total 判耗尽：applyQoderQuota 的 remain/total 只覆盖
		// userQuota + addOnQuota 两个池，dedicatedResourcePackages 的余额既不进
		// remain 也不进 total（qoder_campaign_service.go:443-453 只 Packs++ 与追加
		// Packages 明细）→「remain=0 且 packs>0」完全可能是「套餐用光但专属包还有
		// 余额」。Qoder 只信上游 isQuotaExceeded；它没置位时留给流内错误码
		// （110/111/112/115-119）做事后冷却。
		return false
	case PlatformWorkbuddy:
		// 【PackSizeSum 是唯一可信的正向证据】不能用 Size/Used/Packs：
		// workbuddyAggregateResource 在 TotalDosage > size 时会把 size 抬到历史
		// 累计剂量并用 size-remain 反推 used，于是「Accounts 非空但容量字段全缺省」
		// 的降级读数会被伪造成 remain=0 / used=TotalDosage / size=TotalDosage / packs>0
		// 的「假耗尽」（已用探针实测：两个无容量字段的包 + TotalDosage=10000
		// → remain=0 used=10000 size=10000 packs=2）。PackSizeSum 取的是抬升之前的
		// 逐包容量之和，降级读数下必为 0，因此能区分两者。
		// 旧快照（本字段引入前落的盘）没有 pack_size_sum ⇒ 读为 0 ⇒ 一律放行，
		// 下次探测自然补全，不会永久锁号（另有 24h 新鲜度闸门兜底）。
		return snapshot.PackSizeSum > 0 && snapshot.Used > 0
	default:
		return false
	}
}

// creditSnapshotUtilizationIsReliable 报告本快照的 used/size 是否可用于「用量占比」。
//
// 需同时满足两条（缺一即不可用）：
//  1. **只有 Trae** 满足 used 与 size 同源（均来自未过期权益包聚合）；
//  2. **Credits 必须已耗尽**。Trae 有两个独立的钱袋子：权益包池（size/remain/used）
//     与 status 接口口径的总积分（credits）。used/size 只描述前者，所以
//     「权益包用光但积分还剩」会被算成 100%，而账号其实完全健康。
//     与 creditSnapshotExhausted 的 `Remain > 0 || Credits > 0 → 放行` 保持同源，
//     否则两条通道会对同一账号给出矛盾结论（硬闸门说「没耗尽」、阈值通道说「100%」），
//     管理员设 80% 阈值就会把还有几千积分的号停掉（实测误杀）。
//     积分是「余额」不是「配额池」，本身不存在占比语义，因此这里直接不产出候选。
func creditSnapshotUtilizationIsReliable(snapshot *accountCreditSnapshot) bool {
	return snapshot != nil &&
		snapshot.platform == PlatformTrae &&
		snapshot.Size > 0 &&
		snapshot.Credits <= 0
}

// creditSnapshotRecoveryUntil 积分耗尽的恢复时刻：次日 0 点（跟随全局时区）。
// 与 traeQuotaRecoveryUntil 同一口径——套餐额度按自然日/自然月重置，到点自动
// 复通；若上游仍拒绝，既有流内错误处置会再次落冷却，不会形成反复放行的风暴。
func creditSnapshotRecoveryUntil(now time.Time) time.Time {
	return timezone.StartOfDay(now).AddDate(0, 0, 1)
}

// shouldAutoPauseAccountByCredits 判断账号是否应因积分快照显示耗尽而退出调度。
//
// 放在调度资格判定里（与 shouldAutoPauseGrokAccountByQuota 并列），而不是接进
// 阈值评估器：阈值默认 100 时评估器直接早退（threshold>=100 不评估），若把
// 「硬耗尽」也挂在阈值上，管理员不配就等于不拦 —— 那不是「限定条件自动停调」，
// 而是「必须人工开启」。软阈值（用到 X% 就停）由 creditSnapshotThresholdCandidate
// 单独提供，两条路各管一件事。
func shouldAutoPauseAccountByCredits(account *Account, now time.Time) (bool, openAIQuotaAutoPauseDecision) {
	if account == nil {
		return false, openAIQuotaAutoPauseDecision{}
	}
	snapshot, ok := readAccountCreditSnapshot(account)
	if !ok || !creditSnapshotIsFresh(snapshot, now) {
		return false, openAIQuotaAutoPauseDecision{}
	}
	if !creditSnapshotExhausted(snapshot) {
		return false, openAIQuotaAutoPauseDecision{}
	}
	return true, openAIQuotaAutoPauseDecision{
		window:    creditSnapshotQuotaWindow,
		threshold: 1,
		// 快照没给出可比的百分比时以 1 表示「已到底」，避免 0 让日志与
		// filterStats 读起来像「没用完却被停调」。
		utilization: creditSnapshotUtilization(snapshot),
	}
}

// creditSnapshotThresholdCandidates 把积分快照换算成阈值评估器的候选。
//
// until 用次日 0 点而非 nil：candidateMatchesThreshold 要求 until 非空且严格
// 晚于 now，缺失即永不命中（account_scheduling_threshold_eval.go:413）。额度按
// 自然日/自然月重置，与 creditSnapshotRecoveryUntil 的停调恢复时刻保持一致。
func creditSnapshotThresholdCandidates(account *Account, now time.Time) []*accountSchedulingThresholdCandidate {
	candidate := creditSnapshotThresholdCandidate(account, now)
	if candidate == nil {
		return nil
	}
	return []*accountSchedulingThresholdCandidate{candidate}
}

// creditSnapshotThresholdCandidate 单个候选：快照不可用（平台不匹配 / 无快照 /
// 陈旧 / 企业成员无可查额度 / 占比不可比）时返回 nil，意为「本次不参与阈值停调」。
//
// 实际只会为 Trae 产出（由下面的 reliability 闸门决定）；Qoder 的上游布尔与
// WorkBuddy 的硬耗尽都走 shouldAutoPauseAccountByCredits 那条无条件闸门。
func creditSnapshotThresholdCandidate(account *Account, now time.Time) *accountSchedulingThresholdCandidate {
	if account == nil {
		return nil
	}
	snapshot, ok := readAccountCreditSnapshot(account)
	if !ok || !creditSnapshotIsFresh(snapshot, now) || snapshot.NotApplicable {
		return nil
	}
	// 【只对 Trae 开软阈值】「用量达 X% 停调」要求 used/size 同源可比：
	//  - WorkBuddy 的 size 会被 TotalDosage（历史累计剂量）抬升，used 再由
	//    size-remain 反推 ⇒ 一个包内 5500/剩 1500 的号算出 85%，而真实约 73%，
	//    且 TotalDosage 越用越大 ⇒ 管理员设 80% 会把健康号停掉（既有测试
	//    workbuddy_credits_service_test.go:123 自己就写了这次抬升）。
	//  - Qoder 的 total 漏了专属资源包 ⇒ 占比同样失真。
	// 这两个平台的「耗尽」由 creditSnapshotExhausted 那条硬闸门负责（口径已按平台
	// 收窄）；管理员给它们配的百分比阈值维持不生效，见本函数调用点的说明。
	if !creditSnapshotUtilizationIsReliable(snapshot) {
		return nil
	}
	// 已经耗尽的不走阈值通道：硬耗尽由调度资格里的
	// shouldAutoPauseAccountByCredits 无条件拦住，本通道只负责「软预警」
	// （用到 X% 就提前停调）。两者共用同一份判定输入，不致出现互斥结论。
	until := creditSnapshotRecoveryUntil(now)
	if !until.After(now) {
		return nil
	}
	return &accountSchedulingThresholdCandidate{
		window:      creditSnapshotQuotaWindow,
		scope:       account.Platform,
		usedPercent: creditSnapshotUtilization(snapshot) * 100,
		until:       &until,
	}
}

// creditSnapshotUtilization 用量占比（0~1）。无额度池信息时以「是否耗尽」定标。
func creditSnapshotUtilization(snapshot *accountCreditSnapshot) float64 {
	if snapshot == nil {
		return 0
	}
	if snapshot.Size > 0 {
		ratio := snapshot.Used / snapshot.Size
		if ratio > 1 {
			return 1
		}
		if ratio < 0 {
			return 0
		}
		return ratio
	}
	if creditSnapshotExhausted(snapshot) {
		return 1
	}
	return 0
}
