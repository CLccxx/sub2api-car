<template>
  <div class="min-w-0 space-y-3" data-test="reset-vote-panel">
    <dl class="grid min-w-0 grid-cols-2 gap-x-4 gap-y-3 text-sm">
      <div class="min-w-0">
        <dt class="text-xs text-gray-500 dark:text-gray-400">{{ t('userSubscriptions.sharedUsage.resetCredits') }}</dt>
        <dd class="mt-1 flex min-h-8 flex-wrap items-center gap-x-3 gap-y-1">
          <span data-test="shared-usage-reset-credits" class="break-all font-medium tabular-nums text-gray-900 dark:text-gray-100">{{ creditText }}</span>
          <button
            v-if="canCreate" type="button" data-test="reset-vote-create"
            class="inline-flex min-h-8 items-center gap-1.5 rounded-md text-xs font-medium text-primary-700 hover:text-primary-600 focus-visible:outline focus-visible:outline-2 focus-visible:outline-primary-500 disabled:opacity-50 dark:text-primary-300"
            :disabled="submitting !== null" @click="showConfirmation"
          >
            <Icon name="refresh" size="sm" class="shrink-0" aria-hidden="true" />
            <span class="break-words text-left">{{ t(pendingRequestId ? 'userSubscriptions.resetVote.retry' : 'userSubscriptions.resetVote.create') }}</span>
          </button>
        </dd>
      </div>
      <slot />
    </dl>

    <div v-if="proposal && !expired" class="min-w-0 space-y-2 border-t border-gray-100 pt-3 dark:border-dark-700" aria-live="polite">
      <div class="flex flex-wrap items-center justify-between gap-2 text-sm">
        <span class="font-medium text-gray-800 dark:text-gray-200">{{ statusText }}</span>
        <span data-test="reset-vote-progress" class="inline-flex items-center gap-1.5 tabular-nums text-gray-600 dark:text-gray-300">
          <Icon name="users" size="sm" class="shrink-0" aria-hidden="true" />
          {{ proposal.agreed_count }}/{{ proposal.required_count }} {{ t('userSubscriptions.resetVote.agreed') }}
        </span>
      </div>
      <p v-if="safeReason(proposal.reason)" class="break-words text-xs text-gray-500 dark:text-gray-400">{{ safeReason(proposal.reason) }}</p>
      <div class="flex flex-wrap items-center justify-between gap-x-3 gap-y-2 text-xs">
        <span v-if="proposal.my_vote" class="inline-flex items-center gap-1 text-emerald-700 dark:text-emerald-300">
          <Icon name="checkCircle" size="sm" class="shrink-0" aria-hidden="true" />
          {{ t('userSubscriptions.resetVote.myAgreed') }}
        </span>
        <button
          v-if="canAgree" type="button" data-test="reset-vote-agree"
          class="inline-flex min-h-8 items-center justify-center gap-1.5 rounded-md bg-primary-600 px-3 py-1.5 font-medium text-white hover:bg-primary-700 focus-visible:outline focus-visible:outline-2 focus-visible:outline-primary-500 disabled:cursor-not-allowed disabled:opacity-50"
          :disabled="submitting !== null" @click="agree"
        >
          <Icon name="check" size="sm" class="shrink-0" aria-hidden="true" />
          {{ t('userSubscriptions.resetVote.agree') }}
        </button>
        <span v-if="proposal.status === 'voting' && Number.isFinite(deadline)" class="break-words text-gray-500 dark:text-gray-400">
          {{ t('userSubscriptions.resetVote.deadline') }}
          <time :datetime="proposal.expires_at">{{ formatDateTimeToMinute(proposal.expires_at) }}</time>
        </span>
      </div>
    </div>
    <p v-if="!expired && view?.blocked_reason" class="break-words text-xs text-gray-500 dark:text-gray-400" role="status">{{ safeReason(view.blocked_reason) }}</p>
    <p v-if="!expired && pendingRequestId" class="break-words text-xs text-amber-700 dark:text-amber-300" role="status">{{ t('userSubscriptions.resetVote.pendingRequest') }}</p>
    <p v-if="loadFailed" class="break-words text-xs text-amber-700 dark:text-amber-300" role="status">{{ t('userSubscriptions.resetVote.loadFailed') }}</p>
    <p v-if="actionFailed && !confirmationOpen" class="break-words text-xs text-amber-700 dark:text-amber-300" role="alert">{{ t('userSubscriptions.resetVote.actionFailed') }}</p>

    <BaseDialog
      :show="confirmationOpen" :title="t(pendingRequestId ? 'userSubscriptions.resetVote.retryConfirmTitle' : 'userSubscriptions.resetVote.confirmTitle')" width="narrow"
      :close-on-escape="submitting === null" :show-close-button="submitting === null" @close="dismissConfirmation"
    >
      <div class="space-y-3 text-sm text-gray-700 dark:text-gray-300">
        <p>{{ t(pendingRequestId ? 'userSubscriptions.resetVote.retryConfirmBody' : 'userSubscriptions.resetVote.confirmBody', { required: view?.required_count }) }}</p>
        <p>{{ t('userSubscriptions.resetVote.validFor') }}</p>
        <p v-if="actionFailed" class="break-words text-amber-700 dark:text-amber-300" role="alert">{{ t('userSubscriptions.resetVote.actionFailed') }}</p>
      </div>
      <template #footer>
        <button type="button" class="btn btn-secondary" data-test="reset-vote-dismiss" :disabled="submitting !== null" @click="dismissConfirmation">{{ t('common.cancel') }}</button>
        <button type="button" class="btn btn-primary inline-flex items-center gap-2" data-test="reset-vote-confirm" :disabled="submitting !== null || !canCreate" @click="create">
          <Icon name="check" size="sm" class="shrink-0" aria-hidden="true" />
          {{ t(pendingRequestId ? 'userSubscriptions.resetVote.retryConfirm' : 'userSubscriptions.resetVote.confirm') }}
        </button>
      </template>
    </BaseDialog>
  </div>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { useIntervalFn } from '@vueuse/core'
import { useI18n } from 'vue-i18n'
import subscriptionsAPI, { type ResetVoteView } from '@/api/subscriptions'
import BaseDialog from '@/components/common/BaseDialog.vue'
import Icon from '@/components/icons/Icon.vue'
import { formatDateTimeToMinute } from '@/utils/format'

const props = defineProps<{
  subscriptionId: number
  expiresAt: string | null
  resetCredits: number | null
  usageAvailable: boolean
}>()
const emit = defineEmits<{ completed: [proposalId: number] }>()
const { t } = useI18n()
const view = ref<ResetVoteView | null>(null)
const loadFailed = ref(false)
const actionFailed = ref(false)
const confirmationOpen = ref(false)
const submitting = ref<'create' | 'agree' | null>(null)
const now = ref(Date.now())
let controller: AbortController | null = null
let pollTimer: ReturnType<typeof setTimeout> | undefined
let generation = 0
let disposed = false
const pendingRequestId = ref<string | null>(null)
const completed = new Set<number>()
const terminalStates = ['completed', 'expired', 'invalidated', 'failed']
const activeStates = ['voting', 'executing', 'awaiting_confirmation', 'applying']
const reasonCodes = new Set([
  'not_configured', 'member_count_mismatch', 'account_unavailable', 'no_credit',
  'usage_unavailable', 'reset_in_progress', 'source_changed', 'membership_changed',
  'window_reset', 'admin_reset', 'proposal_expired', 'reset_failed', 'confirmation_pending'
])
useIntervalFn(() => { now.value = Date.now() }, 1000)

const expired = computed(() => props.expiresAt !== null && (!Number.isFinite(Date.parse(props.expiresAt)) || Date.parse(props.expiresAt) <= now.value))
const proposal = computed(() => view.value?.proposal ?? null)
const deadline = computed(() => Date.parse(proposal.value?.expires_at ?? ''))
const hasCredits = computed(() => typeof props.resetCredits === 'number' && Number.isFinite(props.resetCredits) && props.resetCredits > 0)
const creditText = computed(() => typeof props.resetCredits === 'number' && Number.isFinite(props.resetCredits) && props.resetCredits >= 0
  ? props.resetCredits : t('userSubscriptions.sharedUsage.unknown'))
const electorateMatches = computed(() => !!view.value && Number.isInteger(view.value.required_count) && view.value.required_count > 0 && view.value.member_count === view.value.required_count)
const canCreate = computed(() => !expired.value && !loadFailed.value && props.usageAvailable && hasCredits.value && electorateMatches.value &&
  view.value?.can_create && !view.value.blocked_reason && (!proposal.value || terminalStates.includes(proposal.value.status)))
const canAgree = computed(() => !expired.value && !loadFailed.value && electorateMatches.value && proposal.value?.status === 'voting' &&
  proposal.value.required_count === view.value?.required_count && proposal.value.can_agree && !proposal.value.my_vote && Number.isFinite(deadline.value) && deadline.value > now.value)
const statusText = computed(() => {
  const status = proposal.value?.status
  if (!status || ![...activeStates, ...terminalStates].includes(status)) return t('userSubscriptions.resetVote.reasons.unavailable')
  return t(`userSubscriptions.resetVote.states.${status === 'voting' && deadline.value <= now.value ? 'expired' : status}`)
})

function safeReason(reason: string | null): string {
  return reason ? t(`userSubscriptions.resetVote.reasons.${reasonCodes.has(reason) ? reason : 'unavailable'}`) : ''
}

function storageKey(): string {
  return `subscription-reset-vote:${props.subscriptionId}`
}

function saveRequestId(value: string | null) {
  pendingRequestId.value = value
  try {
    if (value) sessionStorage.setItem(storageKey(), value)
    else sessionStorage.removeItem(storageKey())
  } catch { /* In-memory retries still reuse the UUID if storage is unavailable. */ }
}

function requestId(): string {
  if (pendingRequestId.value) return pendingRequestId.value
  const bytes = crypto.getRandomValues(new Uint8Array(16))
  bytes[6] = (bytes[6]! & 0x0f) | 0x40
  bytes[8] = (bytes[8]! & 0x3f) | 0x80
  const hex = Array.from(bytes, byte => byte.toString(16).padStart(2, '0')).join('')
  const id = `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`
  saveRequestId(id)
  return id
}

function stopRequest() {
  clearTimeout(pollTimer)
  controller?.abort()
  generation++
}

function startRequest() {
  stopRequest()
  controller = new AbortController()
  return { signal: controller.signal, current: generation }
}

function isCurrent(current: number, signal: AbortSignal): boolean {
  now.value = Date.now()
  return !disposed && !expired.value && current === generation && !signal.aborted
}

function schedulePoll() {
  clearTimeout(pollTimer)
  if (!disposed && !expired.value) pollTimer = setTimeout(() => { void readStatus() }, 10_000)
}

function acceptStatus(result: ResetVoteView) {
  view.value = result
  loadFailed.value = false
  now.value = Date.now()
  if (result.proposal && activeStates.includes(result.proposal.status)) confirmationOpen.value = false
  if (result.proposal && (result.proposal.my_vote || terminalStates.includes(result.proposal.status))) actionFailed.value = false
  if (result.proposal?.status === 'completed' && !completed.has(result.proposal.id)) {
    completed.add(result.proposal.id)
    emit('completed', result.proposal.id)
  }
}

async function readStatus() {
  now.value = Date.now()
  if (disposed || expired.value || submitting.value) return
  const { signal, current } = startRequest()
  const pendingId = pendingRequestId.value
  try {
    const result = await subscriptionsAPI.getResetVote(props.subscriptionId, pendingId ? { signal, requestId: pendingId } : { signal })
    if (isCurrent(current, signal)) {
      if (pendingId && pendingRequestId.value === pendingId && result.request_resolved === true) {
        saveRequestId(null)
        // Resolving a retry must never turn its open dialog into a new vote intent.
        confirmationOpen.value = false
        actionFailed.value = false
      }
      acceptStatus(result)
    }
  } catch {
    if (isCurrent(current, signal)) loadFailed.value = true
  } finally {
    if (isCurrent(current, signal)) schedulePoll()
  }
}

function showConfirmation() {
  now.value = Date.now()
  if (!canCreate.value || submitting.value) return
  actionFailed.value = false
  confirmationOpen.value = true
}

function dismissConfirmation() {
  if (!submitting.value) confirmationOpen.value = false
}

async function create() {
  now.value = Date.now()
  if (!confirmationOpen.value || !canCreate.value || submitting.value) return
  const { signal, current } = startRequest()
  submitting.value = 'create'
  actionFailed.value = false
  try {
    const result = await subscriptionsAPI.createResetVote(props.subscriptionId, requestId(), { signal })
    if (!isCurrent(current, signal)) return
    saveRequestId(null)
    confirmationOpen.value = false
    acceptStatus(result)
  } catch {
    if (isCurrent(current, signal)) actionFailed.value = true
  } finally {
    if (isCurrent(current, signal)) { submitting.value = null; schedulePoll() }
  }
}

async function agree() {
  now.value = Date.now()
  if (!canAgree.value || !proposal.value || submitting.value) return
  const id = proposal.value.id
  const { signal, current } = startRequest()
  submitting.value = 'agree'
  actionFailed.value = false
  try {
    const result = await subscriptionsAPI.agreeResetVote(props.subscriptionId, id, { signal })
    if (isCurrent(current, signal)) acceptStatus(result)
  } catch {
    if (isCurrent(current, signal)) actionFailed.value = true
  } finally {
    if (isCurrent(current, signal)) { submitting.value = null; schedulePoll() }
  }
}

function reset() {
  stopRequest()
  view.value = null
  loadFailed.value = false
  actionFailed.value = false
  confirmationOpen.value = false
  submitting.value = null
  pendingRequestId.value = null
  completed.clear()
  try {
    const saved = sessionStorage.getItem(storageKey())
    if (saved && /^[\da-f]{8}-[\da-f]{4}-4[\da-f]{3}-[89ab][\da-f]{3}-[\da-f]{12}$/i.test(saved)) pendingRequestId.value = saved
  } catch { /* Storage is optional; status is always fetched from the server. */ }
  void readStatus()
}

watch(() => props.subscriptionId, reset, { immediate: true })
watch(expired, reset)
onBeforeUnmount(() => { disposed = true; stopRequest() })
</script>
