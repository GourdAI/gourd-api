<template>
  <div
    v-if="visible"
    data-test="qoder-credits"
    class="min-w-[220px] space-y-1"
  >
    <!-- 余额行：快照/查询结果直接渲染（悬停看套餐与轮次明细 tooltip）。 -->
    <div class="flex flex-wrap items-center gap-1.5">
      <span
        class="inline-flex items-center gap-0.5 rounded px-1.5 py-0.5 text-[10px] font-medium leading-4 text-gray-700 dark:text-gray-200"
        :title="creditsTooltip"
      >
        <span class="text-gray-500 dark:text-gray-400">{{ t('admin.accounts.qoder.creditsRemain') }}</span>
        <span class="font-semibold tabular-nums" :class="{ 'text-red-600 dark:text-red-400': quotaExceeded }">
          {{ formattedRemain }}
        </span>
      </span>
      <span
        v-if="planText"
        class="text-[10px] leading-4 text-gray-400 dark:text-gray-500"
        :title="planTextTooltip"
      >
        {{ planText }}
      </span>
      <span v-if="realm" class="text-[10px] leading-4 text-gray-400">
        {{ realm === 'global' ? 'global' : 'cn' }}
      </span>
    </div>

    <!-- 操作行：查询 + 领取。领取按钮仅在确有风险时禁用（skipped/fail 仍可重试）。 -->
    <div class="flex flex-wrap items-center gap-1.5">
      <button
        type="button"
        data-test="qoder-credits-refresh"
        class="inline-flex items-center gap-0.5 whitespace-nowrap rounded px-1.5 py-0.5 text-[10px] font-medium leading-4 text-blue-600 transition-colors hover:bg-blue-50 disabled:cursor-not-allowed disabled:opacity-50 dark:text-blue-400 dark:hover:bg-blue-900/30"
        :disabled="loading"
        :title="t('admin.accounts.qoder.creditsRefreshTooltip')"
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
        {{ t('admin.accounts.qoder.creditsRefresh') }}
      </button>
      <button
        type="button"
        data-test="qoder-checkin"
        class="inline-flex items-center gap-0.5 whitespace-nowrap rounded px-1.5 py-0.5 text-[10px] font-medium leading-4 text-emerald-600 transition-colors hover:bg-emerald-50 disabled:cursor-not-allowed disabled:opacity-50 dark:text-emerald-400 dark:hover:bg-emerald-900/30"
        :disabled="checkinLoading || alreadyClaimed"
        :title="claimButtonTooltip"
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
        {{ t('admin.accounts.qoder.claim') }}
      </button>
      <!-- 本轮领取状态：已领绿、失败红、无资格灰。 -->
      <span
        v-if="claimStatusLabel"
        class="text-[10px] leading-4"
        :class="claimStatusClass"
        :title="claimStatusDetail || undefined"
      >
        {{ claimStatusLabel }}
      </span>
    </div>

    <!-- 错误行（查询失败保留快照只显示错误；文案截断 80 字符）。 -->
    <div
      v-if="error"
      data-test="qoder-credits-error"
      class="truncate text-[10px] leading-4 text-red-600 dark:text-red-400"
      :title="error"
    >
      {{ truncatedError }}
    </div>
    <div
      v-if="checkinError"
      data-test="qoder-checkin-error"
      class="truncate text-[10px] leading-4 text-red-600 dark:text-red-400"
      :title="checkinError"
    >
      {{ truncatedCheckinError }}
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { adminAPI } from '@/api/admin'
import type { QoderCreditsResult } from '@/api/admin/qoder'
import type { Account } from '@/types'

const props = defineProps<{
  account: Account
}>()

const { t } = useI18n()

const visible = computed(() => props.account.platform === 'qoder')

const loading = ref(false)
const checkinLoading = ref(false)
const error = ref<string | null>(null)
const checkinError = ref<string | null>(null)
const data = ref<QoderCreditsResult | null>(null)

// 本次操作后的本地覆盖（优先于 extra 快照显示）。
const localClaimStatus = ref('')

// 快照过期窗口（与 WorkBuddyCreditsCell 同口径）。
const SNAPSHOT_STALE_MS = 15 * 60 * 1000

// 自动探测去抖窗口与最近一次自动探测时间（模块级，跨实例共享）。
const AUTO_PROBE_DEBOUNCE_MS = 5 * 60 * 1000
const lastAutoProbeAt = new Map<number, number>()

// 后端 UpdateExtra 写入 qoder_credits 的快照形状（字段与 Result 不同名，独立定义）。
type QoderCreditsSnapshotExtra = {
  remain?: number
  used?: number
  total?: number
  plan_remain?: number
  plan_total?: number
  addon_remain?: number
  addon_total?: number
  packs?: number
  packages?: QoderCreditsResult['packages']
  user_type?: string
  quota_exceeded?: boolean
  fetched_at?: number
  realm?: string
}

// 领取快照 extra 形状（round 是活动轮次日，按 10:00 分界，与自然日不同）。
type QoderCheckinSnapshotExtra = {
  round?: string
  status?: string
  checked_at?: number
  credits?: number
  amount?: number
  grant_id?: string
}

// 活动轮次日：每天 10:00（服务器/本地 UTC+8）开放新一轮，0-10 点仍属前一天轮次。
// 与后端 qoderCampaignRoundDate 保持同一口径，否则会把「本轮已领」显示成「未领」。
const currentRound = (): string => {
  const now = new Date()
  if (now.getHours() < 10) {
    const yesterday = new Date(now.getTime() - 24 * 3600 * 1000)
    return isoDate(yesterday)
  }
  return isoDate(now)
}

const isoDate = (d: Date): string => {
  const month = String(d.getMonth() + 1).padStart(2, '0')
  const day = String(d.getDate()).padStart(2, '0')
  return `${d.getFullYear()}-${month}-${day}`
}

// 从 account.extra 读持久化快照。
const snapshotData = computed<QoderCreditsResult | null>(() => {
  const raw = (props.account.extra as Record<string, unknown> | undefined)?.qoder_credits
  if (!raw || typeof raw !== 'object') return null
  const snap = raw as QoderCreditsSnapshotExtra
  if (typeof snap.remain !== 'number') return null
  return {
    success: true,
    realm: typeof snap.realm === 'string' ? snap.realm : undefined,
    user_type: snap.user_type,
    remaining: snap.remain,
    used: snap.used ?? 0,
    total: snap.total ?? 0,
    plan_remain: snap.plan_remain ?? 0,
    plan_total: snap.plan_total ?? 0,
    addon_remain: snap.addon_remain ?? 0,
    addon_total: snap.addon_total ?? 0,
    packs: snap.packs ?? 0,
    packages: snap.packages,
    quota_exceeded: snap.quota_exceeded ?? false,
    // 快照不存可领取态（它是轮次内瞬时状态，靠查询/领取回执维护）。
    claimable: false,
    fetched_at: snap.fetched_at ?? 0
  }
})

// 从 account.extra 读本轮领取快照（仅同轮有效）。
const snapshotClaimStatus = computed(() => {
  const raw = (props.account.extra as Record<string, unknown> | undefined)?.qoder_checkin
  if (!raw || typeof raw !== 'object') return ''
  const snap = raw as QoderCheckinSnapshotExtra
  if (typeof snap.round !== 'string' || typeof snap.status !== 'string') return ''
  if (snap.round !== currentRound()) return ''
  return snap.status
})

// 后端活动查询结果（claimable / today_claim_status）优先，快照兜底。
const resolved = computed(() => data.value || snapshotData.value)

const claimable = computed(() => resolved.value?.claimable === true)

const claimStatus = computed(() => {
  if (localClaimStatus.value) return localClaimStatus.value
  const fromUpstream = resolved.value?.today_claim_status
  if (fromUpstream) return fromUpstream
  return snapshotClaimStatus.value
})

// 本轮已领（含上游 CLAIMED 与本地快照 ok/already）：禁用领取按钮避免无效点击。
const alreadyClaimed = computed(() => {
  const status = claimStatus.value
  return status === 'ok' || status === 'already'
})

const claimStatusLabel = computed(() => {
  switch (claimStatus.value) {
    case 'ok':
    case 'already':
      return t('admin.accounts.qoder.claimDoneToday')
    case 'fail':
      return t('admin.accounts.qoder.claimFailed')
    case 'skipped':
      return t('admin.accounts.qoder.claimSkipped')
    default:
      return ''
  }
})

const claimStatusDetail = computed(() => {
  if (claimStatus.value === 'fail' || claimStatus.value === 'skipped') {
    return checkinError.value || undefined
  }
  if (alreadyClaimed.value) return t('admin.accounts.qoder.claimDoneTodayHint')
  return undefined
})

const claimStatusClass = computed(() => {
  if (claimStatus.value === 'fail') return 'text-red-600 dark:text-red-400'
  if (claimStatus.value === 'skipped') return 'text-gray-400'
  return 'text-emerald-600 dark:text-emerald-400'
})

// 可领取时按钮提示带上额度（如「领取 100 Credits」）。
const claimButtonTooltip = computed(() => {
  if (alreadyClaimed.value) return t('admin.accounts.qoder.claimDoneTodayHint')
  const amount = resolved.value?.claim_amount
  if (claimable.value && typeof amount === 'number' && amount > 0) {
    return t('admin.accounts.qoder.claimTooltipAmount', { amount })
  }
  return t('admin.accounts.qoder.claimTooltip')
})

const realm = computed(() => resolved.value?.realm || '')

const quotaExceeded = computed(() => resolved.value?.quota_exceeded === true)

const formattedRemain = computed(() => {
  const remain = resolved.value?.remaining
  if (typeof remain !== 'number') return '-'
  return remain.toLocaleString()
})

// 套餐内 / 资源包分解（活动赠送进资源包，是用户最关心的一栏）。
const planText = computed(() => {
  const r = resolved.value
  if (!r) return ''
  const addon = typeof r.addon_remain === 'number' ? r.addon_remain : 0
  if (addon <= 0) return ''
  return `addon ${addon.toLocaleString()}`
})

const planTextTooltip = computed(() => {
  const r = resolved.value
  if (!r) return undefined
  return t('admin.accounts.qoder.creditsBreakdown', {
    plan: (r.plan_remain ?? 0).toLocaleString(),
    planTotal: (r.plan_total ?? 0).toLocaleString(),
    addon: (r.addon_remain ?? 0).toLocaleString(),
    addonTotal: (r.addon_total ?? 0).toLocaleString()
  })
})

// 悬停剩余积分时展示：活动轮次 + 专属包明细 + 更新时间。
const creditsTooltip = computed(() => {
  const resolvedData = resolved.value
  if (!resolvedData) return t('admin.accounts.qoder.creditsEmpty')
  const lines: string[] = []
  const packages = resolvedData.packages
  if (packages && packages.length > 0) {
    lines.push(t('admin.accounts.qoder.creditsPackagesTitle') + ':')
    for (const pkg of packages) {
      const name = pkg.name || '-'
      const remain = typeof pkg.remain === 'number' ? pkg.remain.toLocaleString() : '-'
      const total = typeof pkg.total === 'number' ? pkg.total.toLocaleString() : '-'
      const expiry =
        typeof pkg.expires_at === 'number' && pkg.expires_at > 0
          ? ` (${new Date(pkg.expires_at).toLocaleString()})`
          : ''
      lines.push(`· ${name}: ${remain} / ${total}${expiry}`)
    }
  }
  if (resolvedData.claim_campaign_key) {
    lines.push(
      t('admin.accounts.qoder.campaignRound', {
        key: resolvedData.claim_campaign_key,
        amount: resolvedData.claim_amount ?? 0
      })
    )
  }
  const fetchedAt = resolvedData.fetched_at
  if (typeof fetchedAt === 'number' && fetchedAt > 0) {
    lines.push(
      `${t('admin.accounts.qoder.creditsUpdatedAt')}: ${new Date(fetchedAt * 1000).toLocaleString()}`
    )
  }
  return lines.join('\n')
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

const truncatedCheckinError = computed(() => {
  if (!checkinError.value) return ''
  return checkinError.value.length > 80
    ? `${checkinError.value.slice(0, 80)}...`
    : checkinError.value
})

// 挂载时：先用持久化快照渲染；快照缺失或过期再自动探测一次（模块级去抖）。
onMounted(() => {
  if (!visible.value) return
  data.value = snapshotData.value
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
    const result = await adminAPI.qoder.queryQoderCredits(props.account.id)
    // 失败时保留已渲染的快照（仅显示错误行），成功才覆盖。
    if (result.success) {
      data.value = result
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
    const result = await adminAPI.qoder.checkinQoderAccount(props.account.id)
    localClaimStatus.value = result.status
    if (result.status === 'fail' || result.status === 'skipped') {
      checkinError.value = result.detail || t('common.error')
    }
    // 领取成功（ok/already）后用回执余额刷新显示，缺额度则回退查一次。
    if (result.status === 'ok' || result.status === 'already') {
      if (typeof result.credits === 'number' && data.value) {
        data.value = {
          ...data.value,
          remaining: result.credits,
          today_claim_status: 'already',
          claimable: false
        }
      } else {
        await handleProbe()
      }
    }
  } catch (e) {
    localClaimStatus.value = 'fail'
    checkinError.value = extractErrorMessage(e)
  } finally {
    checkinLoading.value = false
  }
}

// 列表复用组件：切换账号必须复位全部本地态，否则串号。
watch(
  () => props.account.id,
  () => {
    data.value = null
    error.value = null
    checkinError.value = null
    loading.value = false
    checkinLoading.value = false
    localClaimStatus.value = ''
  }
)
</script>
