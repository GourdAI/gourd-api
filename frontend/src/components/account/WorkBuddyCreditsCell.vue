<template>
  <div
    v-if="visible"
    data-test="workbuddy-credits"
    class="min-w-[220px] space-y-1"
  >
    <!-- 积分行：快照/查询结果直接渲染（含套餐明细 tooltip）。 -->
    <div class="flex flex-wrap items-center gap-1.5">
      <!-- 企业成员账号：个人 billing 接口本就没有额度数据，不把 0 当成余额展示。 -->
      <span
        v-if="notApplicable"
        data-test="workbuddy-credits-enterprise"
        class="inline-flex items-center rounded px-1.5 py-0.5 text-[10px] font-medium leading-4 text-gray-500 dark:text-gray-400"
        :title="t('admin.accounts.workbuddy.creditsEnterpriseNATip')"
      >
        {{ t('admin.accounts.workbuddy.creditsEnterpriseNA') }}
      </span>
      <span
        v-else
        data-test="workbuddy-credits-remain"
        class="inline-flex items-center gap-0.5 rounded px-1.5 py-0.5 text-[10px] font-medium leading-4 text-gray-700 dark:text-gray-200"
        :title="packagesTooltip"
      >
        <span class="text-gray-500 dark:text-gray-400">{{ t('admin.accounts.workbuddy.creditsRemain') }}</span>
        <span class="font-semibold tabular-nums">{{ formattedRemain }}</span>
      </span>
      <span v-if="realm" class="text-[10px] leading-4 text-gray-400">
        {{ realm === 'global' ? 'global' : 'cn' }}
      </span>
    </div>

    <!-- 操作行：查询 + 签到。 -->
    <div class="flex flex-wrap items-center gap-1.5">
      <button
        type="button"
        data-test="workbuddy-credits-refresh"
        class="inline-flex items-center gap-0.5 whitespace-nowrap rounded px-1.5 py-0.5 text-[10px] font-medium leading-4 text-blue-600 transition-colors hover:bg-blue-50 disabled:cursor-not-allowed disabled:opacity-50 dark:text-blue-400 dark:hover:bg-blue-900/30"
        :disabled="loading"
        :title="t('admin.accounts.workbuddy.creditsRefreshTooltip')"
        @click="handleProbe()"
      >
        <svg
          class="h-2.5 w-2.5"
          :class="{ 'animate-spin': loading }"
          fill="none"
          stroke="currentColor"
          viewBox="0 0 24 24"
        >
          <path
            stroke-linecap="round"
            stroke-linejoin="round"
            stroke-width="2"
            d="M4 4v5h.582m15.356 2A8.001 8.001 0 004.582 9m0 0H9m11 11v-5h-.581m0 0a8.003 8.003 0 01-15.357-2m15.357 2H15"
          />
        </svg>
        {{ t('admin.accounts.workbuddy.creditsRefresh') }}
      </button>
      <button
        type="button"
        data-test="workbuddy-checkin"
        class="inline-flex items-center gap-0.5 whitespace-nowrap rounded px-1.5 py-0.5 text-[10px] font-medium leading-4 text-emerald-600 transition-colors hover:bg-emerald-50 disabled:cursor-not-allowed disabled:opacity-50 dark:text-emerald-400 dark:hover:bg-emerald-900/30"
        :disabled="checkinLoading || isEnterprise"
        :title="checkinButtonTooltip"
        @click="handleCheckin()"
      >
        <svg
          v-if="checkinLoading"
          class="h-2.5 w-2.5 animate-spin"
          fill="none"
          stroke="currentColor"
          viewBox="0 0 24 24"
        >
          <path
            stroke-linecap="round"
            stroke-linejoin="round"
            stroke-width="2"
            d="M4 4v5h.582m15.356 2A8.001 8.001 0 004.582 9m0 0H9m11 11v-5h-.581m0 0a8.003 8.003 0 01-15.357-2m15.357 2H15"
          />
        </svg>
        {{ t('admin.accounts.workbuddy.checkin') }}
      </button>
      <button
        type="button"
        data-test="workbuddy-activity-run"
        class="inline-flex items-center gap-0.5 whitespace-nowrap rounded px-1.5 py-0.5 text-[10px] font-medium leading-4 text-indigo-600 transition-colors hover:bg-indigo-50 disabled:cursor-not-allowed disabled:opacity-50 dark:text-indigo-400 dark:hover:bg-indigo-900/30"
        :disabled="activityLoading"
        :title="t('admin.accounts.workbuddy.activityRunTooltip')"
        @click="handleRunActivity()"
      >
        <svg
          v-if="activityLoading"
          class="h-2.5 w-2.5 animate-spin"
          fill="none"
          stroke="currentColor"
          viewBox="0 0 24 24"
        >
          <path
            stroke-linecap="round"
            stroke-linejoin="round"
            stroke-width="2"
            d="M4 4v5h.582m15.356 2A8.001 8.001 0 004.582 9m0 0H9m11 11v-5h-.581m0 0a8.003 8.003 0 01-15.357-2m15.357 2H15"
          />
        </svg>
        {{ t('admin.accounts.workbuddy.activityRun') }}
      </button>
      <button
        type="button"
        data-test="workbuddy-travel-run"
        class="inline-flex items-center gap-0.5 whitespace-nowrap rounded px-1.5 py-0.5 text-[10px] font-medium leading-4 text-amber-600 transition-colors hover:bg-amber-50 disabled:cursor-not-allowed disabled:opacity-50 dark:text-amber-400 dark:hover:bg-amber-900/30"
        :disabled="travelLoading"
        :title="t('admin.accounts.workbuddy.travelRunTooltip')"
        @click="handleRunTravel()"
      >
        <svg
          v-if="travelLoading"
          class="h-2.5 w-2.5 animate-spin"
          fill="none"
          stroke="currentColor"
          viewBox="0 0 24 24"
        >
          <path
            stroke-linecap="round"
            stroke-linejoin="round"
            stroke-width="2"
            d="M4 4v5h.582m15.356 2A8.001 8.001 0 004.582 9m0 0H9m11 11v-5h-.581m0 0a8.003 8.003 0 01-15.357-2m15.357 2H15"
          />
        </svg>
        {{ t('admin.accounts.workbuddy.travelRun') }}
      </button>
      <!-- 今日签到状态（快照或本次操作后的结果）：ok/already 绿、fail 红。 -->
      <span
        v-if="checkinStatusLabel"
        class="text-[10px] leading-4"
        :class="checkinStatusClass"
        :title="checkinStatusDetail || undefined"
      >
        {{ checkinStatusLabel }}
      </span>
    </div>

    <!-- 今日任务快照：活跃上报 / 猫猫旅行（account.extra 快照或本次运行结果）。 -->
    <div
      v-if="activityStatusLabel || travelStatusLabel"
      class="flex flex-wrap items-center gap-x-1.5 gap-y-1"
    >
      <span
        v-if="activityStatusLabel"
        data-test="workbuddy-activity-snapshot"
        class="text-[10px] leading-4"
        :class="activityStatusClass"
        :title="activityStatusDetail || undefined"
      >
        {{ activityStatusLabel }}
      </span>
      <span
        v-if="travelStatusLabel"
        data-test="workbuddy-travel-snapshot"
        class="text-[10px] leading-4"
        :class="travelStatusClass"
        :title="travelStatusDetail || undefined"
      >
        {{ travelStatusLabel }}
      </span>
    </div>

    <!-- 错误行（查询失败保留快照只显示错误；文案截断 80 字符）。 -->
    <div
      v-if="error"
      class="truncate text-[10px] leading-4 text-red-600 dark:text-red-400"
      :title="error"
    >
      {{ truncatedError }}
    </div>

    <!-- 任务运行失败行（截断 80 字符，与查询错误行同风格）。 -->
    <div
      v-if="activityError"
      data-test="workbuddy-activity-error"
      class="truncate text-[10px] leading-4 text-red-600 dark:text-red-400"
      :title="activityError"
    >
      {{ truncatedActivityError }}
    </div>
    <div
      v-if="travelError"
      data-test="workbuddy-travel-error"
      class="truncate text-[10px] leading-4 text-red-600 dark:text-red-400"
      :title="travelError"
    >
      {{ truncatedTravelError }}
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { adminAPI } from '@/api/admin'
import type {
  WorkBuddyActivityRunResult,
  WorkBuddyCreditsResult,
  WorkBuddyTravelRunResult
} from '@/api/admin/workbuddy'
import type { Account } from '@/types'

const props = defineProps<{
  account: Account
}>()

const { t } = useI18n()

const visible = computed(() => props.account.platform === 'workbuddy')

const loading = ref(false)
const checkinLoading = ref(false)
const activityLoading = ref(false)
const travelLoading = ref(false)
const error = ref<string | null>(null)
const checkinError = ref<string | null>(null)
const activityError = ref<string | null>(null)
const travelError = ref<string | null>(null)
const data = ref<WorkBuddyCreditsResult | null>(null)

// 签到结果的本地覆盖（本次操作后优先于 extra 快照显示）。
const localCheckinStatus = ref('')

// 任务运行结果的本地覆盖（本次跑任务/旅行后优先于 extra 快照显示）。
const localActivityStatus = ref<WorkBuddyActivityRunResult | null>(null)
const localTravelStatus = ref<WorkBuddyTravelRunResult | null>(null)

// 快照过期窗口（与 CNProviderQuotaCell 同口径）。
const SNAPSHOT_STALE_MS = 15 * 60 * 1000

// 自动探测去抖窗口与最近一次自动探测时间（模块级，跨实例共享）。
const AUTO_PROBE_DEBOUNCE_MS = 5 * 60 * 1000
const lastAutoProbeAt = new Map<number, number>()

type WorkBuddyCreditsSnapshotExtra = {
  remain?: number
  used?: number
  size?: number
  packs?: number
  packages?: WorkBuddyCreditsResult['packages']
  fetched_at?: number
  realm?: string
  not_applicable?: boolean
  enterprise?: boolean
  /** 后端按配置时区（Asia/Shanghai）判定的今日签到态，已过「仅当日」闸门。 */
  today_checkin_status?: string
}

// 任务快照 extra 形状（后端 UpdateExtra 写入 workbuddy_activity / workbuddy_travel）。
type WorkBuddyActivitySnapshotExtra = {
  date?: string
  streak_days?: number
  reports?: number
  reward_tier?: string
  credit_granted?: number
  lottery_drawn?: boolean
  ran_at?: number
  ok?: boolean
}

type WorkBuddyTravelSnapshotExtra = {
  date?: string
  action?: string
  state?: string
  buddy_name?: string
  reward_credit?: number
  ran_at?: number
  ok?: boolean
}

// 本地日期（YYYY-MM-DD）；快照 date 与此相符才视为今日新鲜。
const todayLocalDate = () => {
  const now = new Date()
  const month = String(now.getMonth() + 1).padStart(2, '0')
  const day = String(now.getDate()).padStart(2, '0')
  return `${now.getFullYear()}-${month}-${day}`
}

// 从 account.extra 读持久化快照（后端 UpdateExtra 写入的 workbuddy_credits）。
const snapshotData = computed<WorkBuddyCreditsResult | null>(() => {
  const raw = (props.account.extra as Record<string, unknown> | undefined)?.workbuddy_credits
  if (!raw || typeof raw !== 'object') return null
  const snap = raw as WorkBuddyCreditsSnapshotExtra
  if (typeof snap.remain !== 'number') return null
  return {
    success: true,
    realm: typeof snap.realm === 'string' ? snap.realm : undefined,
    remain: snap.remain,
    used: snap.used,
    size: snap.size,
    packs: snap.packs,
    packages: snap.packages,
    fetched_at: snap.fetched_at,
    not_applicable: snap.not_applicable,
    enterprise: snap.enterprise,
    today_checkin_status: snap.today_checkin_status
  }
})

// 从 account.extra 读今日签到快照（workbuddy_checkin）。
//
// 【P0：旧口径把「昨日已签到」显示成「今日已签到」】旧实现拿
// `new Date().toISOString().slice(0,10)`（**UTC 日期**）去比后端写入的
// `workbuddy_checkin.date`（**Asia/Shanghai 日期串**），还宽化到「或昨天」。两个
// 口径在中国大陆浏览器上必然错位：本地 00:00-07:59 时 UTC 仍是昨天，昨日快照
// 直接命中 today；本地 08:00 后 UTC 变今天，昨日快照又命中 yesterday 宽化分支
// ——结果是**整天**都显示绿色「今日已签到」，真实漏签被掩盖、直接损失每日积分。
// 现行做法：优先读后端随积分快照下发的 `today_checkin_status`（已按权威时区
// 判定且做过「仅当日」闸门）；只有旧版后端没下发该字段时，才降级用本地日期
// 严格相等比对（不再宽化昨天，宁可不显示也不误报）。
const snapshotCheckinStatus = computed(() => {
  const fromCreditsSnapshot = snapshotData.value?.today_checkin_status
  if (typeof fromCreditsSnapshot === 'string' && fromCreditsSnapshot) {
    return fromCreditsSnapshot
  }
  const raw = (props.account.extra as Record<string, unknown> | undefined)?.workbuddy_checkin
  if (!raw || typeof raw !== 'object') return ''
  const snap = raw as { date?: string; status?: string }
  if (typeof snap.date !== 'string' || typeof snap.status !== 'string') return ''
  // 降级路径：日期串必须与浏览器本地今日严格相等（与 Trae 组件同口径）。
  if (snap.date !== todayLocalDate()) return ''
  return snap.status
})

const checkinStatus = computed(() => localCheckinStatus.value || snapshotCheckinStatus.value)

const checkinStatusLabel = computed(() => {
  // 企业账号：修复前可能已写入 today 的 ok/already 快照（当时确实在尝试签到），
  // 继续展示「今日已签到」会直接 contradict「无个人签到体系」，一律不显示。
  if (isEnterprise.value) return ''
  switch (checkinStatus.value) {
    case 'ok':
    case 'already':
      return t('admin.accounts.workbuddy.checkinDoneToday')
    case 'fail':
      return t('admin.accounts.workbuddy.checkinFailed')
    case 'skipped':
      return t('admin.accounts.workbuddy.checkinSkipped')
    default:
      return ''
  }
})

const checkinStatusDetail = computed(() => {
  if (checkinStatus.value === 'fail') return checkinError.value || undefined
  if (checkinStatus.value === 'skipped') return checkinError.value || undefined
  return undefined
})

const checkinStatusClass = computed(() => {
  if (checkinStatus.value === 'fail') {
    return 'text-red-600 dark:text-red-400'
  }
  if (checkinStatus.value === 'skipped') {
    return 'text-gray-400'
  }
  return 'text-emerald-600 dark:text-emerald-400'
})

// 从 account.extra 读今日活跃上报快照（workbuddy_activity，仅当日有效）。
const snapshotActivity = computed<WorkBuddyActivitySnapshotExtra | null>(() => {
  const raw = (props.account.extra as Record<string, unknown> | undefined)?.workbuddy_activity
  if (!raw || typeof raw !== 'object') return null
  const snap = raw as WorkBuddyActivitySnapshotExtra
  if (typeof snap.date !== 'string' || snap.date !== todayLocalDate()) return null
  return snap
})

// 从 account.extra 读今日猫猫旅行快照（workbuddy_travel，仅当日有效）。
const snapshotTravel = computed<WorkBuddyTravelSnapshotExtra | null>(() => {
  const raw = (props.account.extra as Record<string, unknown> | undefined)?.workbuddy_travel
  if (!raw || typeof raw !== 'object') return null
  const snap = raw as WorkBuddyTravelSnapshotExtra
  if (typeof snap.date !== 'string' || snap.date !== todayLocalDate()) return null
  return snap
})

// 今日活跃上报展示文案：「连登 N 天 · 已发 M 条」。
const activityStatusLabel = computed(() => {
  const streak = localActivityStatus.value?.streak_days ?? snapshotActivity.value?.streak_days
  const reports = localActivityStatus.value?.reports ?? snapshotActivity.value?.reports
  const fresh = localActivityStatus.value !== null || snapshotActivity.value !== null
  if (!fresh) return ''
  if (typeof streak !== 'number' && typeof reports !== 'number') return ''
  return t('admin.accounts.workbuddy.activitySnapshot', {
    days: typeof streak === 'number' ? streak : '-',
    reports: typeof reports === 'number' ? reports : '-'
  })
})

// 今日活跃上报状态色：失败红、其余绿。
const activityStatusClass = computed(() => {
  const failed = localActivityStatus.value
    ? !localActivityStatus.value.success
    : snapshotActivity.value?.ok === false
  return failed ? 'text-red-600 dark:text-red-400' : 'text-emerald-600 dark:text-emerald-400'
})

const activityStatusDetail = computed(() => {
  if (localActivityStatus.value?.detail) return localActivityStatus.value.detail
  if (activityError.value) return activityError.value
  return undefined
})

// 旅行动作 → i18n 文案（与后端 action 取值逐字对应）。
const travelActionLabel = (action: string): string => {
  switch (action) {
    case 'adopt':
      return t('admin.accounts.workbuddy.travelActionAdopt')
    case 'claim':
      return t('admin.accounts.workbuddy.travelActionClaim')
    case 'depart':
      return t('admin.accounts.workbuddy.travelActionDepart')
    case 'skip':
      return t('admin.accounts.workbuddy.travelActionSkip')
    default:
      return ''
  }
}

// 今日旅行展示文案：动作 + 猫名 + 奖励积分。
const travelStatusLabel = computed(() => {
  const local = localTravelStatus.value
  const snap = snapshotTravel.value
  if (!local && !snap) return ''
  const action = local?.action || snap?.action || ''
  const actionLabel = travelActionLabel(action)
  if (!actionLabel) return ''
  const parts = [actionLabel]
  const buddyName = local?.buddy_name || snap?.buddy_name
  if (buddyName) parts.push(buddyName)
  const credit = local?.reward_credit ?? snap?.reward_credit
  if (typeof credit === 'number' && credit > 0) parts.push(`+${credit}`)
  return parts.join(' · ')
})

// 今日旅行状态色：失败红、其余绿。
const travelStatusClass = computed(() => {
  const failed = localTravelStatus.value
    ? !localTravelStatus.value.success
    : snapshotTravel.value?.ok === false
  return failed ? 'text-red-600 dark:text-red-400' : 'text-emerald-600 dark:text-emerald-400'
})

const travelStatusDetail = computed(() => {
  if (localTravelStatus.value?.detail) return localTravelStatus.value.detail
  if (travelError.value) return travelError.value
  return undefined
})

const realm = computed(() => data.value?.realm || snapshotData.value?.realm || '')

// 企业成员账号不适用判定：查询结果或快照任一标了 not_applicable 即生效。
// （不能只看 enterprise：部分企业账号仍能拿到个人套餐，那时应正常展余额。）
const notApplicable = computed(
  () => data.value?.not_applicable === true || snapshotData.value?.not_applicable === true
)

// 企业账号判定（与后端签到 D5 门控同口径：只要持 enterprise_id 就无个人签到）。
const isEnterprise = computed(
  () => data.value?.enterprise === true || snapshotData.value?.enterprise === true
)

const formattedRemain = computed(() => {
  const remain = data.value?.remain ?? snapshotData.value?.remain
  if (typeof remain !== 'number') return '-'
  return remain.toLocaleString()
})

// 套餐明细 + 更新时间 tooltip（悬停剩余积分数字时展示）。
const packagesTooltip = computed(() => {
  const resolved = data.value || snapshotData.value
  if (!resolved) return t('admin.accounts.workbuddy.creditsEmpty')
  const lines: string[] = []
  const packages = resolved.packages
  if (packages && packages.length > 0) {
    lines.push(t('admin.accounts.workbuddy.creditsPackagesTitle') + ':')
    for (const pkg of packages) {
      const name = pkg.package_name || '-'
      const remain = typeof pkg.remain === 'number' ? pkg.remain.toLocaleString() : '-'
      const size = typeof pkg.size === 'number' ? pkg.size.toLocaleString() : '-'
      const end = pkg.cycle_end_time ? ` (${pkg.cycle_end_time})` : ''
      lines.push(`· ${name}: ${remain} / ${size}${end}`)
    }
  }
  const fetchedAt = resolved.fetched_at
  if (typeof fetchedAt === 'number' && fetchedAt > 0) {
    lines.push(
      `${t('admin.accounts.workbuddy.creditsUpdatedAt')}: ${new Date(fetchedAt * 1000).toLocaleString()}`
    )
  }
  return lines.join('\n')
})

const checkinButtonTooltip = computed(() => {
  if (isEnterprise.value) return t('admin.accounts.workbuddy.checkinEnterpriseTooltip')
  return checkinError.value && checkinStatus.value === 'skipped'
    ? checkinError.value
    : t('admin.accounts.workbuddy.checkinTooltip')
})

const extractErrorMessage = (e: unknown): string => {
  const err = e as {
    message?: string
    reason?: string
    response?: { data?: { message?: string; error?: string } }
  }
  return (
    err?.message ||
    err?.reason ||
    err?.response?.data?.message ||
    err?.response?.data?.error ||
    t('common.error')
  )
}

const truncatedError = computed(() => {
  if (!error.value) return ''
  return error.value.length > 80 ? `${error.value.slice(0, 80)}...` : error.value
})

const truncatedActivityError = computed(() => {
  if (!activityError.value) return ''
  return activityError.value.length > 80
    ? `${activityError.value.slice(0, 80)}...`
    : activityError.value
})

const truncatedTravelError = computed(() => {
  if (!travelError.value) return ''
  return travelError.value.length > 80 ? `${travelError.value.slice(0, 80)}...` : travelError.value
})

// 挂载时：先用持久化快照渲染；快照缺失或过期再自动探测一次。
onMounted(() => {
  if (!visible.value) return
  data.value = snapshotData.value
  // 快照过期判定：无 fetched_at 或超过 staleness 窗口。
  const fetchedAt = snapshotData.value?.fetched_at
  const fresh = typeof fetchedAt === 'number' && Date.now() - fetchedAt * 1000 <= SNAPSHOT_STALE_MS
  if (fresh) return
  const last = lastAutoProbeAt.get(props.account.id) ?? 0
  if (Date.now() - last < AUTO_PROBE_DEBOUNCE_MS) return
  lastAutoProbeAt.set(props.account.id, Date.now())
  handleProbe()
})

const handleProbe = async () => {
  if (loading.value) return
  loading.value = true
  error.value = null
  try {
    const result = await adminAPI.workbuddy.queryWorkBuddyCredits(props.account.id)
    // 失败时保留已渲染的快照（仅显示错误行），成功才覆盖。
    if (result.success) {
      data.value = result
      // 签到态以本次查询回执为权威：后端已按配置时区算好 today_checkin_status，
      // 空串也必须覆盖本地态，否则旧快照误报的「今日已签到」点多少次查询都洗不掉。
      if (!localCheckinStatus.value) {
        localCheckinStatus.value = result.today_checkin_status ?? ''
      }
    } else {
      error.value = result.error || t('common.error')
    }
  } catch (e) {
    error.value = extractErrorMessage(e)
  } finally {
    loading.value = false
  }
}

const handleCheckin = async () => {
  if (checkinLoading.value) return
  checkinLoading.value = true
  checkinError.value = null
  try {
    const result = await adminAPI.workbuddy.checkinWorkBuddyAccount(props.account.id)
    localCheckinStatus.value = result.status
    if (result.status === 'fail' || result.status === 'skipped') {
      checkinError.value = result.detail || t('common.error')
    }
    // 签到成功（ok/already）后刷新积分显示。
    if (result.status === 'ok' || result.status === 'already') {
      if (typeof result.credits === 'number' && data.value) {
        data.value = { ...data.value, remain: result.credits }
      } else {
        await handleProbe()
      }
    }
  } catch (e) {
    localCheckinStatus.value = 'fail'
    checkinError.value = extractErrorMessage(e)
  } finally {
    checkinLoading.value = false
  }
}

// 手动跑「对话活跃上报」任务：结果直接覆盖本地展示（与 localCheckinStatus 同模式）。
const handleRunActivity = async () => {
  if (activityLoading.value) return
  activityLoading.value = true
  activityError.value = null
  try {
    const result = await adminAPI.workbuddy.runWorkBuddyActivity(props.account.id)
    localActivityStatus.value = result
    if (!result.success) {
      activityError.value = result.detail || t('common.error')
    }
  } catch (e) {
    localActivityStatus.value = null
    activityError.value = extractErrorMessage(e)
  } finally {
    activityLoading.value = false
  }
}

// 手动跑「猫猫旅行巡检」任务：结果直接覆盖本地展示（与 localCheckinStatus 同模式）。
const handleRunTravel = async () => {
  if (travelLoading.value) return
  travelLoading.value = true
  travelError.value = null
  try {
    const result = await adminAPI.workbuddy.runWorkBuddyTravel(props.account.id)
    localTravelStatus.value = result
    if (!result.success) {
      travelError.value = result.detail || t('common.error')
    }
  } catch (e) {
    localTravelStatus.value = null
    travelError.value = extractErrorMessage(e)
  } finally {
    travelLoading.value = false
  }
}

watch(
  () => props.account.id,
  () => {
    data.value = null
    error.value = null
    loading.value = false
    checkinLoading.value = false
    activityLoading.value = false
    travelLoading.value = false
    checkinError.value = null
    activityError.value = null
    travelError.value = null
    localCheckinStatus.value = ''
    localActivityStatus.value = null
    localTravelStatus.value = null
  }
)
</script>
