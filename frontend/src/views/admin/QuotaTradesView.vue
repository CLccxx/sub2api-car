<template>
  <AppLayout>
    <TablePageLayout>
      <template #filters>
        <div class="space-y-3">
          <p class="text-sm text-gray-500 dark:text-gray-400">{{ t('quotaTrades.admin.description') }}</p>
          <form class="flex flex-wrap items-end gap-3" @submit.prevent="applyFilters">
            <label class="text-sm text-gray-700 dark:text-gray-300">
              {{ t('quotaTrades.admin.status') }}
              <select v-model="filters.status" class="input mt-1" data-test="filter-status">
                <option value="">{{ t('quotaTrades.admin.allStatuses') }}</option>
                <option v-for="status in statuses" :key="status" :value="status">{{ t(`quotaTrades.status.${status}`) }}</option>
              </select>
            </label>
            <label v-for="key in idFilters" :key="key" class="text-sm text-gray-700 dark:text-gray-300">
              {{ t(`quotaTrades.admin.${key}`) }}
              <input v-model="filters[key]" type="number" min="1" step="1" class="input mt-1 w-36" :data-test="`filter-${key}`" />
            </label>
            <button type="submit" class="btn btn-primary" :disabled="busy">{{ t('quotaTrades.admin.filter') }}</button>
            <button type="button" class="btn btn-secondary" :disabled="loading || busy" data-test="refresh-trades" @click="load()">{{ t('common.refresh') }}</button>
          </form>
          <p v-if="error" role="alert" class="text-sm text-red-600 dark:text-red-400">{{ error }}</p>
        </div>
      </template>
      <template #table>
        <DataTable :columns="columns" :data="items" :loading="loading">
          <template #cell-id="{ row }"><span class="font-mono">#{{ row.id }}</span></template>
          <template #cell-group="{ row }">
            <div>{{ row.group_name }} #{{ row.group_id }}</div>
            <div class="text-xs text-gray-500">{{ t('quotaTrades.admin.account_id') }}: #{{ row.account_id }} · {{ t('quotaTrades.admin.cycle') }}: #{{ row.cycle_id }}</div>
          </template>
          <template #cell-seller="{ row }">{{ row.seller_name || '—' }} #{{ row.seller_user_id }}</template>
          <template #cell-buyer="{ row }">{{ row.buyer_user_id != null ? `${row.buyer_name || '—'} #${row.buyer_user_id}` : '—' }}</template>
          <template #cell-amount="{ row }">
            <div>${{ row.amount_usd.toFixed(2) }}</div>
            <div v-if="row.sale_price_cny != null" class="text-xs text-gray-500">{{ t('quotaTrades.salePrice') }} ¥{{ row.sale_price_cny.toFixed(2) }}</div>
          </template>
          <template #cell-status="{ row }">{{ t(`quotaTrades.status.${row.status}`) }}</template>
          <template #cell-created_at="{ row }">
            <div>{{ formatDateTimeToMinute(row.created_at) }}</div>
            <div v-if="row.reserved_at" class="text-xs text-gray-500">{{ t('quotaTrades.reservedAt') }}: {{ formatDateTimeToMinute(row.reserved_at) }}</div>
            <div v-if="row.completed_at" class="text-xs text-gray-500">{{ t('quotaTrades.completedAt') }}: {{ formatDateTimeToMinute(row.completed_at) }}</div>
          </template>
          <template #cell-actions="{ row }">
            <div class="flex flex-wrap gap-2" :data-trade-id="row.id">
              <button type="button" class="btn btn-secondary btn-sm" data-test="trade-events" @click="showEvents(row)">{{ t('quotaTrades.admin.events') }}</button>
              <button v-if="row.status === 'reserved'" type="button" class="btn btn-primary btn-sm" :disabled="busy || loading || !!error" data-test="admin-confirm" @click="selectAction(row, 'confirm')">{{ t('quotaTrades.admin.confirm') }}</button>
              <button v-if="row.status === 'reserved'" type="button" class="btn btn-secondary btn-sm" :disabled="busy || loading || !!error" data-test="admin-cancel-reservation" @click="selectAction(row, 'cancel-reservation')">{{ t('quotaTrades.admin.cancelReservation') }}</button>
              <button v-if="row.status === 'open'" type="button" class="btn btn-secondary btn-sm" :disabled="busy || loading || !!error" data-test="admin-withdraw" @click="selectAction(row, 'withdraw')">{{ t('quotaTrades.withdraw') }}</button>
            </div>
          </template>
        </DataTable>
      </template>
      <template #pagination>
        <Pagination v-if="total > 0" :total="total" :page="page" :page-size="pageSize" @update:page="changePage" @update:page-size="changePageSize" />
      </template>
    </TablePageLayout>
    <ConfirmDialog :show="!!pending" :title="t('quotaTrades.admin.actionTitle')" :message="pending ? t(`quotaTrades.admin.messages.${pending.action}`, { id: pending.trade.id, amount: pending.trade.amount_usd.toFixed(2) }) : ''" :busy="busy" :confirm-disabled="pending?.action === 'confirm' && !paymentVerified" :danger="true" @cancel="pending = null" @confirm="executeAction">
      <p v-if="pending?.trade.sale_price_cny != null" class="text-sm font-semibold text-gray-900 dark:text-gray-100">{{ t('quotaTrades.salePrice') }} ¥{{ pending.trade.sale_price_cny.toFixed(2) }}</p>
      <label v-if="pending?.action === 'confirm'" class="flex items-start gap-2 text-sm text-gray-700 dark:text-gray-300">
        <input v-model="paymentVerified" type="checkbox" class="mt-1" data-test="admin-payment-verified" />
        {{ t('quotaTrades.admin.paymentVerified') }}
      </label>
    </ConfirmDialog>
    <BaseDialog :show="!!eventTrade" :title="t('quotaTrades.admin.eventsTitle', { id: eventTrade?.id })" @close="closeEvents">
      <p v-if="eventsLoading" class="text-sm text-gray-500">{{ t('common.loading') }}</p>
      <div v-else-if="eventsError" class="space-y-3">
        <p role="alert" class="text-sm text-red-600 dark:text-red-400">{{ eventsError }}</p>
        <button type="button" class="btn btn-secondary" data-test="retry-events" @click="eventTrade && showEvents(eventTrade)">{{ t('common.refresh') }}</button>
      </div>
      <p v-else-if="!events.length" class="text-sm text-gray-500">{{ t('quotaTrades.admin.noEvents') }}</p>
      <ol v-else class="space-y-4">
        <li v-for="event in events" :key="event.id" class="rounded-lg border border-gray-200 p-3 text-sm text-gray-700 dark:border-dark-600 dark:text-gray-300">
          <div class="font-medium">{{ eventAction(event.action) }}</div>
          <div>{{ event.actor_name || t('quotaTrades.admin.systemActor') }} <span v-if="event.actor_user_id != null">#{{ event.actor_user_id }}</span> <span v-if="event.is_admin">({{ t('quotaTrades.admin.adminActor') }})</span></div>
          <div v-if="event.buyer_user_id != null">{{ t('quotaTrades.buyer') }}: #{{ event.buyer_user_id }}</div>
          <time class="text-xs text-gray-500">{{ formatDateTimeToMinute(event.created_at) }}</time>
        </li>
      </ol>
    </BaseDialog>
  </AppLayout>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, reactive, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { useAppStore } from '@/stores/app'
import * as api from '@/api/admin/quotaTrades'
import type { AdminQuotaTradeFilters, QuotaTrade, QuotaTradeEvent, QuotaTradeStatus } from '@/types/quotaTrade'
import type { Column } from '@/components/common/types'
import AppLayout from '@/components/layout/AppLayout.vue'
import TablePageLayout from '@/components/layout/TablePageLayout.vue'
import DataTable from '@/components/common/DataTable.vue'
import Pagination from '@/components/common/Pagination.vue'
import ConfirmDialog from '@/components/common/ConfirmDialog.vue'
import BaseDialog from '@/components/common/BaseDialog.vue'
import { formatDateTimeToMinute } from '@/utils/format'
import { getPersistedPageSize } from '@/composables/usePersistedPageSize'

const { t, te } = useI18n()
const app = useAppStore()
const statuses: QuotaTradeStatus[] = ['open', 'reserved', 'completed', 'withdrawn', 'expired']
const idFilters = ['group_id', 'account_id', 'user_id'] as const
const filters = reactive({ status: '' as QuotaTradeStatus | '', group_id: '', account_id: '', user_id: '' })
let appliedFilters: AdminQuotaTradeFilters = {}
const items = ref<QuotaTrade[]>([])
const total = ref(0)
const page = ref(1)
const pageSize = ref(getPersistedPageSize())
const loading = ref(false)
const busy = ref(false)
const error = ref('')
const columns = computed<Column[]>(() => [
  { key: 'id', label: t('quotaTrades.admin.order') },
  { key: 'group', label: t('quotaTrades.admin.group') },
  { key: 'seller', label: t('quotaTrades.seller') },
  { key: 'buyer', label: t('quotaTrades.buyer') },
  { key: 'amount', label: t('quotaTrades.amount') },
  { key: 'status', label: t('quotaTrades.admin.status') },
  { key: 'created_at', label: t('quotaTrades.createdAt') },
  { key: 'actions', label: t('quotaTrades.admin.actions') }
])
type Action = 'confirm' | 'cancel-reservation' | 'withdraw'
const pending = ref<{ trade: QuotaTrade; action: Action } | null>(null)
const paymentVerified = ref(false)
const eventTrade = ref<QuotaTrade | null>(null)
const events = ref<QuotaTradeEvent[]>([])
const eventsLoading = ref(false)
const eventsError = ref('')
let controller: AbortController | null = null
let eventController: AbortController | null = null
let generation = 0
let eventGeneration = 0
let disposed = false
const message = (cause: unknown) => (cause as { message?: string })?.message || t('quotaTrades.failed')
async function load() {
  if (disposed || busy.value) return
  controller?.abort()
  controller = new AbortController()
  const signal = controller.signal
  const request = ++generation
  loading.value = true
  try {
    const result = await api.listQuotaTrades({ ...appliedFilters, page: page.value, page_size: pageSize.value }, { signal })
    if (disposed || signal.aborted || request !== generation) return
    items.value = result.items
    total.value = result.total
    error.value = ''
  } catch (cause) {
    if (!disposed && !signal.aborted && request === generation) error.value = message(cause)
  } finally {
    if (request === generation) loading.value = false
  }
}
function applyFilters() {
  const next: AdminQuotaTradeFilters = {}
  if (filters.status) next.status = filters.status
  for (const key of idFilters) {
    if (filters[key] === '') continue
    const id = Number(filters[key])
    if (!Number.isSafeInteger(id) || id < 1) {
      app.showError(t('quotaTrades.admin.invalidId'))
      return
    }
    next[key] = id
  }
  appliedFilters = next
  page.value = 1
  void load()
}
function changePage(value: number) {
  if (busy.value) return
  page.value = value
  void load()
}
function changePageSize(value: number) {
  if (busy.value) return
  pageSize.value = value
  page.value = 1
  void load()
}
function selectAction(trade: QuotaTrade, action: Action) {
  if (busy.value || loading.value || error.value) return
  paymentVerified.value = false
  pending.value = { trade, action }
}
async function executeAction() {
  if (!pending.value || busy.value || (pending.value.action === 'confirm' && !paymentVerified.value)) return
  const { trade, action } = pending.value
  busy.value = true
  try {
    const fn = { confirm: api.confirmQuotaTrade, 'cancel-reservation': api.cancelQuotaTradeReservation, withdraw: api.withdrawQuotaTrade }[action]
    await fn(trade.id)
    if (disposed) return
    app.showSuccess(t('quotaTrades.success'))
    if (eventTrade.value?.id === trade.id) void showEvents(trade)
  } catch (cause) {
    if (!disposed) app.showError(message(cause))
  } finally {
    pending.value = null
    busy.value = false
    if (!disposed) await load()
  }
}
function eventAction(action: string) {
  const key = `quotaTrades.eventActions.${action.replace(/-/g, '_')}`
  return te(key) ? t(key) : action
}
async function showEvents(trade: QuotaTrade) {
  eventController?.abort()
  eventController = new AbortController()
  const signal = eventController.signal
  const request = ++eventGeneration
  eventTrade.value = trade
  events.value = []
  eventsError.value = ''
  eventsLoading.value = true
  try {
    const result = await api.getQuotaTradeEvents(trade.id, { signal })
    if (!disposed && !signal.aborted && request === eventGeneration) events.value = result
  } catch (cause) {
    if (!disposed && !signal.aborted && request === eventGeneration) eventsError.value = message(cause)
  } finally {
    if (request === eventGeneration) eventsLoading.value = false
  }
}
function closeEvents() {
  eventTrade.value = null
  eventGeneration++
  eventController?.abort()
}
onMounted(() => void load())
onBeforeUnmount(() => {
  disposed = true
  generation++
  eventGeneration++
  controller?.abort()
  eventController?.abort()
})
</script>
