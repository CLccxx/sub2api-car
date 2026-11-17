<template>
  <section class="space-y-4 border-t border-gray-100 p-4 dark:border-dark-700" :aria-label="t('quotaTrades.title')">
    <div class="flex items-center justify-between gap-3">
      <h4 class="text-sm font-semibold text-gray-900 dark:text-white">{{ t('quotaTrades.title') }}</h4>
      <button type="button" class="btn btn-secondary btn-sm" :disabled="loading || busy" data-test="refresh-trades" @click="load()">{{ t('common.refresh') }}</button>
    </div>
    <p class="text-xs text-gray-500 dark:text-gray-400">{{ t('quotaTrades.description') }}</p>
    <p v-if="error" role="alert" class="text-sm text-red-600 dark:text-red-400">{{ error }}</p>
    <p v-if="!snapshot && loading" class="text-sm text-gray-500">{{ t('common.loading') }}</p>
    <template v-if="snapshot">
      <dl class="grid grid-cols-2 gap-3 rounded-lg bg-gray-50 p-3 text-xs dark:bg-dark-900 sm:grid-cols-3">
        <div v-for="key in quotaKeys" :key="key">
          <dt class="text-gray-500 dark:text-gray-400">{{ t(`quotaTrades.quota.${key}`) }}</dt>
          <dd class="mt-1 font-medium text-gray-900 dark:text-gray-100">${{ snapshot.quota[key].toFixed(2) }}</dd>
        </div>
      </dl>
      <p v-if="!snapshot.enabled" class="text-sm text-amber-700 dark:text-amber-300">{{ t('quotaTrades.disabled') }}</p>
      <form v-if="snapshot.enabled && active" class="space-y-3" @submit.prevent="requestCreate">
        <div>
          <div class="flex flex-wrap items-baseline gap-x-3 gap-y-1">
            <label :for="`trade-amount-${subscriptionId}`" class="text-sm text-gray-700 dark:text-gray-300">{{ t('quotaTrades.amount') }}</label>
            <span class="text-xs text-gray-500 dark:text-gray-400" data-test="suggested-quota-price">{{ suggestedPrice === null ? t('quotaTrades.suggestedPriceUnavailable') : t('quotaTrades.suggestedPrice', { estimate: pricingEstimate, price: suggestedPrice }) }}</span>
          </div>
          <input :id="`trade-amount-${subscriptionId}`" v-model="amount" type="number" min="0.01" step="0.01" inputmode="decimal" class="input mt-1" :placeholder="t('quotaTrades.amountPlaceholder')" :disabled="busy" data-test="trade-amount" />
        </div>
        <p class="text-xs text-gray-500 dark:text-gray-400">{{ t('quotaTrades.availableRule') }}</p>
        <label class="flex items-start gap-2 text-xs text-gray-600 dark:text-gray-300">
          <input v-model="accepted" type="checkbox" class="mt-0.5 rounded" :disabled="busy" data-test="trade-terms" />
          <span>{{ t('quotaTrades.terms.notice') }}</span>
        </label>
        <button type="submit" class="btn btn-primary btn-sm" :disabled="!canCreate" data-test="create-trade">{{ t('quotaTrades.create') }}</button>
      </form>
      <p class="rounded-lg bg-amber-50 p-3 text-xs text-amber-800 dark:bg-amber-900/20 dark:text-amber-200">{{ t('quotaTrades.rules') }}</p>
      <div class="flex gap-1 border-b border-gray-200 dark:border-dark-600" role="tablist" :aria-label="t('quotaTrades.tabs.label')">
        <button v-for="view in tradeViews" :id="`trade-tab-${subscriptionId}-${view}`" :key="view" type="button" role="tab" :aria-selected="activeView === view" :aria-controls="`trade-list-${subscriptionId}`" :disabled="busy" class="border-b-2 px-4 py-2 text-sm font-medium transition-colors" :class="activeView === view ? 'border-primary-500 text-primary-600 dark:text-primary-400' : 'border-transparent text-gray-500 hover:text-gray-700 dark:text-gray-400 dark:hover:text-gray-200'" :data-test="`trade-tab-${view}`" @click="changeView(view)">{{ t(`quotaTrades.tabs.${view}`) }}</button>
      </div>
      <div :id="`trade-list-${subscriptionId}`" role="tabpanel" :aria-labelledby="`trade-tab-${subscriptionId}-${activeView}`" :aria-busy="loading">
      <p v-if="loadedView !== activeView" class="py-3 text-center text-sm text-gray-500">{{ error ? t('quotaTrades.failed') : t('common.loading') }}</p>
      <p v-else-if="snapshot.items.length === 0" class="py-3 text-center text-sm text-gray-500">{{ t(`quotaTrades.tabs.${activeView}Empty`) }}</p>
      <ul v-else class="space-y-3">
        <li v-for="trade in snapshot.items" :key="trade.id" class="space-y-2 rounded-lg border border-gray-200 p-3 text-sm dark:border-dark-600" :data-trade-id="trade.id">
          <div class="flex flex-wrap items-center justify-between gap-2">
            <span class="font-semibold text-gray-900 dark:text-gray-100">#{{ trade.id }} · ${{ trade.amount_usd.toFixed(2) }} {{ t('quotaTrades.serviceQuota') }}<span v-if="trade.sale_price_cny != null" class="ml-3 text-primary-600 dark:text-primary-400" data-test="trade-sale-price">{{ t('quotaTrades.salePrice') }} ¥{{ trade.sale_price_cny.toFixed(2) }}</span></span>
            <span class="rounded bg-gray-100 px-2 py-1 text-xs text-gray-700 dark:bg-dark-600 dark:text-gray-200">{{ t(`quotaTrades.status.${trade.status}`) }}</span>
          </div>
          <div class="grid grid-cols-2 text-xs text-gray-500 dark:text-gray-400">
            <div class="min-w-0 space-y-1 break-words pr-3" data-test="trade-seller-details">
              <p>{{ t('quotaTrades.seller') }}: {{ trade.seller_name || `#${trade.seller_user_id}` }} <span v-if="trade.seller_user_id === myId">({{ t('quotaTrades.mine') }})</span></p>
              <p>{{ t('quotaTrades.createdAt') }}: {{ formatDateTimeToMinute(trade.created_at) }}</p>
              <p v-if="trade.completed_at" data-test="trade-completed-at">{{ t('quotaTrades.completedAt') }}: {{ formatDateTimeToMinute(trade.completed_at) }}</p>
            </div>
            <div class="min-w-0 self-start h-9 space-y-1 break-words border-l border-gray-300 pl-3 dark:border-gray-600" data-test="trade-buyer-details">
              <p>{{ t('quotaTrades.buyer') }}: {{ trade.buyer_user_id != null ? (trade.buyer_name || `#${trade.buyer_user_id}`) : '—' }} <span v-if="trade.buyer_user_id != null && trade.buyer_user_id === myId">({{ t('quotaTrades.mine') }})</span></p>
              <p>{{ t('quotaTrades.reservedAt') }}: {{ trade.reserved_at ? formatDateTimeToMinute(trade.reserved_at) : '—' }}</p>
            </div>
          </div>
          <div v-if="activeView === 'current' && trade.can_action !== false && (canReserve(trade) || (trade.status === 'reserved' && trade.seller_user_id === myId) || (trade.status === 'open' && trade.seller_user_id === myId))" class="flex flex-wrap gap-2">
            <div class="flex flex-wrap gap-2">
              <button v-if="canReserve(trade)" type="button" class="btn btn-primary btn-sm" :disabled="!canAct" data-test="reserve-trade" @click="selectAction(trade, 'reserve')">{{ t('quotaTrades.reserve') }}</button>
              <button v-if="trade.status === 'reserved' && trade.seller_user_id === myId" type="button" class="btn btn-primary btn-sm" :disabled="!canAct" data-test="confirm-trade" @click="selectAction(trade, 'confirm')">{{ t('quotaTrades.confirm') }}</button>
              <button v-if="trade.status === 'open' && trade.seller_user_id === myId" type="button" class="btn btn-secondary btn-sm" :disabled="!canAct" data-test="withdraw-trade" @click="selectAction(trade, 'withdraw')">{{ t('quotaTrades.withdraw') }}</button>
            </div>
          </div>
        </li>
      </ul>
      <Pagination v-if="loadedView === activeView && snapshot.total > 0" :total="snapshot.total" :page="page" :page-size="pageSize" :show-page-size-selector="false" @update:page="changePage" />
      </div>
    </template>
    <ConfirmDialog :show="!!pendingCreate" :title="t('quotaTrades.createConfirmation.title')" :message="pendingCreate ? t('quotaTrades.createConfirmation.message', { amount: pendingCreate.amount.toFixed(2) }) : ''" :busy="busy" :confirm-disabled="!canConfirmCreate" @cancel="pendingCreate = null" @confirm="create">
      <dl v-if="pendingCreate" class="space-y-2 rounded-lg bg-gray-50 p-3 text-sm dark:bg-dark-900" data-test="create-price-summary">
        <div class="flex flex-wrap justify-between gap-2">
          <dt class="text-gray-500 dark:text-gray-400">{{ t('quotaTrades.createConfirmation.amount') }}</dt>
          <dd class="font-medium text-gray-900 dark:text-gray-100">{{ pendingCreate.amount.toFixed(2) }}$</dd>
        </div>
        <div class="flex flex-wrap justify-between gap-2">
          <dt class="text-gray-500 dark:text-gray-400">{{ t('quotaTrades.createConfirmation.unitPrice') }}</dt>
          <dd class="font-medium text-gray-900 dark:text-gray-100" data-test="create-unit-price">{{ pendingCreate.unitPrice ?? '—' }}</dd>
        </div>
        <div class="flex flex-wrap justify-between gap-2">
          <dt class="text-gray-500 dark:text-gray-400">{{ t('quotaTrades.createConfirmation.totalPrice') }}</dt>
          <dd class="font-semibold text-gray-900 dark:text-gray-100" data-test="create-total-price">{{ pendingCreate.totalPrice === null ? '—' : `${pendingCreate.unitPrice} × ${pendingCreate.amount.toFixed(2)} = ${pendingCreate.totalPrice}` }}</dd>
        </div>
      </dl>
      <label class="block text-sm text-gray-700 dark:text-gray-300">
        {{ t('quotaTrades.createConfirmation.salePrice') }}
        <input v-model="salePrice" type="number" min="0.01" max="10000000000" step="0.01" inputmode="decimal" class="input mt-1" :placeholder="t('quotaTrades.createConfirmation.salePricePlaceholder')" :disabled="busy" :aria-invalid="salePrice !== '' && !validSalePrice" data-test="trade-sale-price-input" />
      </label>
      <p v-if="salePrice !== '' && !validSalePrice" role="alert" class="text-xs text-red-600 dark:text-red-400">{{ t('quotaTrades.createConfirmation.invalidSalePrice') }}</p>
      <p class="text-xs text-gray-500 dark:text-gray-400">{{ t('quotaTrades.createConfirmation.priceHint') }}</p>
    </ConfirmDialog>
    <ConfirmDialog :show="!!pending" :title="pending ? t(`quotaTrades.${pending.action}`) : ''" :message="pending ? t(`quotaTrades.messages.${pending.action}`, { id: pending.trade.id, amount: pending.trade.amount_usd.toFixed(2) }) : ''" :busy="busy" :confirm-disabled="pending?.action === 'confirm' && !paymentReceived" :danger="pending?.action === 'withdraw'" @cancel="pending = null" @confirm="executeAction">
      <p v-if="pending?.trade.sale_price_cny != null" class="text-sm font-semibold text-gray-900 dark:text-gray-100" data-test="action-sale-price">{{ t('quotaTrades.salePrice') }} ¥{{ pending.trade.sale_price_cny.toFixed(2) }}</p>
      <label v-if="pending?.action === 'confirm'" class="flex items-start gap-2 text-sm text-gray-700 dark:text-gray-300">
        <input v-model="paymentReceived" type="checkbox" class="mt-1" data-test="payment-received" />
        {{ t('quotaTrades.paymentReceived') }}
      </label>
    </ConfirmDialog>
  </section>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { useAuthStore } from '@/stores/auth'
import { useAppStore } from '@/stores/app'
import * as api from '@/api/quotaTrades'
import type { QuotaTrade, QuotaTradeQuota, QuotaTradeView, SubscriptionQuotaTrades } from '@/types/quotaTrade'
import ConfirmDialog from '@/components/common/ConfirmDialog.vue'
import Pagination from '@/components/common/Pagination.vue'
import { formatDateTimeToMinute } from '@/utils/format'

const props = defineProps<{ subscriptionId: number; active: boolean; refreshKey?: number; accountEstimate?: number | null }>()
const emit = defineEmits<{ changed: []; 'quota-updated': [quota: QuotaTradeQuota] }>()
const { t } = useI18n()
const auth = useAuthStore()
const app = useAppStore()
const myId = computed(() => auth.user?.id)
const snapshot = ref<SubscriptionQuotaTrades | null>(null)
const error = ref('')
const loading = ref(false)
const busy = ref(false)
const amount = ref('')
const accepted = ref(false)
const salePrice = ref('')
const numericSalePrice = computed(() => Number(salePrice.value))
const validSalePrice = computed(() => Number.isFinite(numericSalePrice.value) && numericSalePrice.value > 0 && numericSalePrice.value <= 10_000_000_000 && /^\d+(?:\.\d{1,2})?$/.test(String(salePrice.value)))
const quotaKeys = ['base_limit_usd', 'adjustment_usd', 'frozen_usd', 'used_usd', 'available_usd'] as const
const tradeViews = ['current', 'history'] as const
const activeView = ref<QuotaTradeView>('current')
const loadedView = ref<QuotaTradeView | null>(null)
const pages = ref<Record<QuotaTradeView, number>>({ current: 1, history: 1 })
const page = computed({ get: () => pages.value[activeView.value], set: value => { pages.value[activeView.value] = value } })
const pageSize = 20
const pending = ref<{ trade: QuotaTrade; action: 'reserve' | 'confirm' | 'withdraw' } | null>(null)
const paymentReceived = ref(false)
const pendingCreate = ref<{ subscriptionId: number; amount: number; unitPrice: string | null; totalPrice: string | null } | null>(null)
let createAttempt: { amount: number; salePrice: number; requestId: string } | null = null
let controller: AbortController | null = null
let timer: ReturnType<typeof setTimeout> | undefined
let generation = 0
let disposed = false
// Use the raw account estimate when available; 1200 is only a pricing fallback.
const pricingEstimate = computed(() => {
  const estimate = props.accountEstimate
  return typeof estimate === 'number' && Number.isFinite(estimate) && estimate > 0 ? estimate : 1200
})
const suggestedPrice = computed(() => {
  const price = 1400 / (pricingEstimate.value * 4)
  if (!Number.isFinite(price) || price <= 0) return null
  return price >= 0.0001 ? price.toFixed(4) : price.toPrecision(4)
})
const numericAmount = computed(() => Number(amount.value))
const canAct = computed(() => !busy.value && !loading.value && !error.value && !!snapshot.value?.enabled && props.active)
const validAmount = computed(() => Number.isFinite(numericAmount.value) && numericAmount.value > 0 && numericAmount.value <= 10_000_000_000 && /^\d+(?:\.\d{1,2})?$/.test(String(amount.value)))
const canCreate = computed(() => canAct.value && accepted.value && validAmount.value && numericAmount.value < (snapshot.value?.quota.available_usd ?? 0))
const canConfirmCreate = computed(() => canAct.value && accepted.value && validSalePrice.value && pendingCreate.value !== null &&
  pendingCreate.value.subscriptionId === props.subscriptionId && pendingCreate.value.amount < (snapshot.value?.quota.available_usd ?? 0))
function requestCreate() {
  if (!canCreate.value || pending.value || pendingCreate.value) return
  salePrice.value = ''
  const unitPrice = suggestedPrice.value
  const total = unitPrice === null ? null : Number(unitPrice) * numericAmount.value
  pendingCreate.value = {
    subscriptionId: props.subscriptionId,
    amount: numericAmount.value,
    unitPrice,
    totalPrice: total !== null && Number.isFinite(total) ? total.toFixed(2) : null
  }
}
function canReserve(trade: QuotaTrade) {
  return trade.can_action !== false && trade.status === 'open' && myId.value != null && trade.seller_user_id !== myId.value
}
function message(cause: unknown) {
  return (cause as { message?: string })?.message || t('quotaTrades.failed')
}
async function load() {
  if (disposed || busy.value) return
  clearTimeout(timer)
  controller?.abort()
  controller = new AbortController()
  const signal = controller.signal
  const request = ++generation
  const view = activeView.value
  loading.value = true
  try {
    const result = await api.listQuotaTrades(props.subscriptionId, { page: page.value, page_size: pageSize, view }, { signal })
    if (disposed || signal.aborted || request !== generation) return
    snapshot.value = result
    loadedView.value = view
    error.value = ''
    // Disabled history-only markets may have no current-cycle entitlement at all.
    if (result.enabled) emit('quota-updated', result.quota)
  } catch (cause) {
    if (!disposed && !signal.aborted && request === generation) error.value = message(cause)
  } finally {
    if (!disposed && request === generation) {
      loading.value = false
      timer = setTimeout(() => void load(), 30_000)
    }
  }
}
function changeView(view: QuotaTradeView) {
  if (busy.value || activeView.value === view) return
  activeView.value = view
  pending.value = null
  void load()
}
function changePage(value: number) {
  if (busy.value) return
  page.value = value
  void load()
}
async function create() {
  if (!canConfirmCreate.value || !pendingCreate.value) return
  const { subscriptionId, amount: saleAmount } = pendingCreate.value
  const confirmedPrice = numericSalePrice.value
  busy.value = true
  clearTimeout(timer)
  try {
    // Keep this key after an ambiguous network failure: retrying must not freeze twice.
    if (!createAttempt || createAttempt.amount !== saleAmount || createAttempt.salePrice !== confirmedPrice) {
      createAttempt = { amount: saleAmount, salePrice: confirmedPrice, requestId: crypto.randomUUID() }
    }
    await api.createQuotaTrade(subscriptionId, { amount_usd: createAttempt.amount, sale_price_cny: createAttempt.salePrice, request_id: createAttempt.requestId, accepted_terms: true })
    if (disposed || subscriptionId !== props.subscriptionId) return
    createAttempt = null
    salePrice.value = ''
    pendingCreate.value = null
    amount.value = ''
    accepted.value = false
    activeView.value = 'current'
    page.value = 1
    app.showSuccess(t('quotaTrades.success'))
    emit('changed')
  } catch (cause) {
    if (!disposed && subscriptionId === props.subscriptionId) app.showError(message(cause))
  } finally {
    busy.value = false
    if (!disposed) await load()
  }
}
function selectAction(trade: QuotaTrade, action: 'reserve' | 'confirm' | 'withdraw') {
  if (!canAct.value || trade.can_action === false || activeView.value !== 'current' || loadedView.value !== 'current' || pendingCreate.value) return
  paymentReceived.value = false
  pending.value = { trade, action }
}
async function executeAction() {
  if (busy.value || !pending.value || (pending.value.action === 'confirm' && !paymentReceived.value)) return
  const { trade, action } = pending.value
  const subscriptionId = props.subscriptionId
  busy.value = true
  clearTimeout(timer)
  controller?.abort()
  try {
    const fn = { reserve: api.reserveQuotaTrade, confirm: api.confirmQuotaTrade, withdraw: api.withdrawQuotaTrade }[action]
    await fn(subscriptionId, trade.id)
    if (disposed || subscriptionId !== props.subscriptionId) return
    app.showSuccess(t('quotaTrades.success'))
    emit('changed')
  } catch (cause) {
    if (!disposed && subscriptionId === props.subscriptionId) app.showError(message(cause))
  } finally {
    pending.value = null
    busy.value = false
    if (!disposed) await load()
  }
}
watch(() => [props.subscriptionId, props.refreshKey], (_, previous) => {
  if (previous && previous[0] !== props.subscriptionId) {
    snapshot.value = null
    loadedView.value = null
    activeView.value = 'current'
    pages.value = { current: 1, history: 1 }
    pending.value = null
    pendingCreate.value = null
    amount.value = ''
    salePrice.value = ''
    accepted.value = false
    createAttempt = null
  }
  void load()
}, { immediate: true })
onBeforeUnmount(() => {
  disposed = true
  generation++
  controller?.abort()
  clearTimeout(timer)
})
</script>
