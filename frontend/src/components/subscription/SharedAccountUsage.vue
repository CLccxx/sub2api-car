<template>
  <section class="min-w-0 space-y-4 border-t border-gray-100 p-4 dark:border-dark-700" data-test="shared-account-usage" :aria-label="t('userSubscriptions.sharedUsage.title')" :aria-busy="loading">
    <div class="flex min-w-0 items-center justify-between gap-3">
      <div class="flex min-w-0 items-center gap-2">
        <Icon name="chartBar" size="sm" class="shrink-0 text-gray-500 dark:text-gray-400" aria-hidden="true" />
        <h4 class="text-sm font-semibold text-gray-900 dark:text-gray-100">{{ t('userSubscriptions.sharedUsage.title') }}</h4>
      </div>
      <button
        type="button" data-test="shared-usage-refresh"
        class="inline-flex h-8 w-8 shrink-0 items-center justify-center rounded-md text-gray-500 transition-colors hover:bg-gray-100 focus-visible:outline focus-visible:outline-2 focus-visible:outline-primary-500 disabled:cursor-not-allowed disabled:opacity-40 dark:text-gray-400 dark:hover:bg-dark-700"
        :disabled="loading || expired || cooldownSeconds > 0" :title="refreshTitle" :aria-label="refreshTitle" @click="load"
      >
        <Icon name="refresh" size="sm" :class="{ 'animate-spin': loading }" aria-hidden="true" />
      </button>
    </div>

    <p v-if="loading" class="text-sm text-gray-500 dark:text-gray-400" role="status">{{ t('common.loading') }}</p>
    <p v-else-if="stateMessage" class="break-words text-sm text-amber-700 dark:text-amber-300" role="status">{{ stateMessage }}</p>

    <template v-if="showUsageMetrics">
      <div class="space-y-2">
        <div class="flex items-center justify-between gap-3 text-sm">
          <span class="font-medium text-gray-700 dark:text-gray-300">{{ t('userSubscriptions.sharedUsage.sevenDay') }}</span>
          <span data-test="shared-usage-percent" class="font-semibold tabular-nums text-gray-900 dark:text-gray-100">{{ percentText }}</span>
        </div>
        <div
          v-if="utilization !== null" role="progressbar" :aria-label="t('userSubscriptions.sharedUsage.sevenDay')"
          :aria-valuenow="Math.min(utilization, 100)" :aria-valuetext="percentText" :aria-valuemin="0" :aria-valuemax="100"
          class="h-2 overflow-hidden rounded-full bg-gray-100 dark:bg-dark-600"
        >
          <div :class="['h-full transition-all duration-300', barClass]" :style="{ width: `${Math.min(utilization, 100)}%` }" />
        </div>
        <div v-else class="h-2 rounded-full bg-gray-100 dark:bg-dark-600" />
      </div>

      <dl class="grid min-w-0 grid-cols-2 gap-x-4 gap-y-4 text-sm">
        <div class="min-w-0">
          <dt class="flex items-center gap-1 text-xs text-gray-500 dark:text-gray-400">
            {{ t('userSubscriptions.sharedUsage.estimatedTotalCost') }}
            <HelpTooltip :content="t('userSubscriptions.sharedUsage.estimateHint')" />
          </dt>
          <dd data-test="shared-usage-estimate" class="mt-1 break-all font-medium tabular-nums text-gray-900 dark:text-gray-100">{{ estimateText }}</dd>
        </div>
        <div class="min-w-0">
          <dt class="text-xs text-gray-500 dark:text-gray-400">{{ t('userSubscriptions.sharedUsage.resetIn') }}</dt>
          <dd data-test="shared-usage-countdown" class="mt-1 break-words font-medium tabular-nums text-gray-900 dark:text-gray-100">{{ countdown }}</dd>
        </div>
      </dl>
    </template>
    <ResetVotePanel
      v-if="!expired"
      :subscription-id="subscriptionId" :expires-at="expiresAt" :reset-credits="usage?.reset_credits ?? null"
      :usage-available="!loading && !failed && !rolledOver && usage?.status === 'ready'"
      @completed="emit('reset-completed', $event)"
    >
      <div v-if="showUsageMetrics" class="min-w-0">
        <dt class="text-xs text-gray-500 dark:text-gray-400">{{ t('userSubscriptions.sharedUsage.points') }}</dt>
        <dd data-test="shared-usage-points" class="mt-1 break-all font-medium tabular-nums text-gray-900 dark:text-gray-100">{{ pointsText }}</dd>
      </div>
    </ResetVotePanel>
    <p v-if="showUsageMetrics && usage?.updated_at" class="break-words text-xs text-gray-400 dark:text-gray-500">
      {{ t('userSubscriptions.sharedUsage.updatedAt', { time: formatDateTimeToMinute(usage.updated_at) }) }}
    </p>
  </section>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { useIntervalFn } from '@vueuse/core'
import { useI18n } from 'vue-i18n'
import subscriptionsAPI, { type SharedAccountUsage } from '@/api/subscriptions'
import Icon from '@/components/icons/Icon.vue'
import HelpTooltip from '@/components/common/HelpTooltip.vue'
import ResetVotePanel from './ResetVotePanel.vue'
import { formatDateTimeToMinute } from '@/utils/format'

const props = withDefaults(defineProps<{ subscriptionId: number; expiresAt: string | null; refreshKey?: number }>(), { refreshKey: 0 })
const emit = defineEmits<{ 'reset-completed': [proposalId: number] }>()
const { t } = useI18n()
const usage = ref<SharedAccountUsage | null>(null)
const loading = ref(false)
const failed = ref(false)
const retryAt = ref(0)
const now = ref(Date.now())
let controller: AbortController | null = null
let generation = 0
let loadedSubscriptionId: number | null = null
useIntervalFn(() => { now.value = Date.now() }, 1000)

const expired = computed(() => props.expiresAt !== null && Date.parse(props.expiresAt) <= now.value)
const showUsageMetrics = computed(() => !expired.value && (!usage.value || ['ready', 'no_data', 'unavailable'].includes(usage.value.status)))
const unknown = computed(() => t('userSubscriptions.sharedUsage.unknown'))
const cooldownSeconds = computed(() => Math.max(0, Math.ceil((retryAt.value - now.value) / 1000)))
const refreshTitle = computed(() => cooldownSeconds.value > 0
  ? t('userSubscriptions.sharedUsage.refreshIn', { seconds: cooldownSeconds.value }) : t('common.refresh'))
const rolledOver = computed(() => {
  const reset = usage.value?.seven_day?.resets_at
  return !!reset && Date.parse(reset) <= now.value
})
const utilization = computed(() => {
  const value = usage.value?.seven_day?.utilization
  return !rolledOver.value && typeof value === 'number' && Number.isFinite(value) && value >= 0 ? value : null
})
const percentText = computed(() => utilization.value === null ? unknown.value : `${Number(utilization.value.toFixed(1))}%`)
const barClass = computed(() => (utilization.value ?? 0) >= 90 ? 'bg-red-500' : (utilization.value ?? 0) >= 75 ? 'bg-amber-500' : 'bg-emerald-500')
const estimateText = computed(() => {
  const value = usage.value?.seven_day?.estimated_total_cost
  return !rolledOver.value && typeof value === 'number' && Number.isFinite(value) && value >= 0 ? `$${value.toFixed(2)}` : unknown.value
})
const countdown = computed(() => {
  const reset = Date.parse(usage.value?.seven_day?.resets_at ?? '')
  if (!Number.isFinite(reset)) return unknown.value
  if (reset <= now.value) return t('userSubscriptions.sharedUsage.awaitingRefresh')
  const minutes = Math.ceil((reset - now.value) / 60_000)
  const days = Math.floor(minutes / 1440)
  const hours = Math.floor((minutes % 1440) / 60)
  return days > 0 ? `${days}d ${hours}h ${minutes % 60}m` : hours > 0 ? `${hours}h ${minutes % 60}m` : `${minutes}m`
})
const pointsText = computed(() => {
  const credits = usage.value?.credits
  if (!credits) return unknown.value
  if (credits.unlimited) return t('userSubscriptions.sharedUsage.unlimited')
  if (!credits.has_credits) return '0'
  if (credits.balance !== null && credits.balance.trim() !== '' && Number.isFinite(Number(credits.balance)) && Number(credits.balance) >= 0) return credits.balance
  return t('userSubscriptions.sharedUsage.available')
})
const stateMessage = computed(() => {
  if (expired.value) return t('userSubscriptions.sharedUsage.states.expired')
  if (failed.value) return t('userSubscriptions.sharedUsage.states.unavailable')
  if (!usage.value || usage.value.status === 'ready') return ''
  return t(`userSubscriptions.sharedUsage.states.${usage.value.status}`)
})

async function load() {
  if (loading.value || expired.value || cooldownSeconds.value > 0) return
  controller?.abort()
  controller = new AbortController()
  const signal = controller.signal
  const request = ++generation
  loading.value = true
  failed.value = false
  try {
    const result = await subscriptionsAPI.getSharedAccountUsage(props.subscriptionId, { signal })
    if (signal.aborted || request !== generation) return
    usage.value = result
    now.value = Date.now()
    const next = Date.parse(result.refresh_available_at ?? '')
    retryAt.value = Number.isFinite(next) ? next : now.value + 60_000
  } catch {
    if (signal.aborted || request !== generation) return
    usage.value = null
    failed.value = true
    retryAt.value = Date.now() + 60_000
  } finally {
    if (request === generation) loading.value = false
  }
}

watch(() => [props.subscriptionId, props.refreshKey], () => {
  controller?.abort()
  generation++
  if (props.subscriptionId !== loadedSubscriptionId) usage.value = null
  loadedSubscriptionId = props.subscriptionId
  failed.value = false
  loading.value = false
  retryAt.value = 0
  void load()
}, { immediate: true })
watch(expired, value => {
  if (!value) { void load(); return }
  controller?.abort()
  generation++
  usage.value = null
  loading.value = false
})
onBeforeUnmount(() => { generation++; controller?.abort() })
</script>
