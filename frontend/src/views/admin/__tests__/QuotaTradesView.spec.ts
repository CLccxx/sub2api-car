import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { enableAutoUnmount, flushPromises, mount } from '@vue/test-utils'
import QuotaTradesView from '../QuotaTradesView.vue'
import type { QuotaTrade } from '@/types/quotaTrade'

const mocks = vi.hoisted(() => ({
  listQuotaTrades: vi.fn(), getQuotaTradeEvents: vi.fn(), confirmQuotaTrade: vi.fn(), cancelQuotaTradeReservation: vi.fn(), withdrawQuotaTrade: vi.fn(), showError: vi.fn(), showSuccess: vi.fn()
}))
vi.mock('@/api/admin/quotaTrades', () => mocks)
vi.mock('@/stores/app', () => ({ useAppStore: () => mocks }))
vi.mock('vue-i18n', async importOriginal => ({ ...await importOriginal<typeof import('vue-i18n')>(), useI18n: () => ({ t: (key: string) => key, te: () => true }) }))
vi.mock('@/composables/usePersistedPageSize', () => ({ getPersistedPageSize: () => 20 }))
enableAutoUnmount(afterEach)
const trade = (status: QuotaTrade['status'] = 'reserved'): QuotaTrade => ({
  id: 42, group_id: 1, group_name: 'Carpool', account_id: 3, cycle_id: 'cycle-4', seller_user_id: 1, seller_name: 'Seller', seller_subscription_id: 12,
  buyer_user_id: 2, buyer_name: 'Buyer', amount_usd: 50, status, created_at: '2026-11-01T00:00:00Z', reserved_at: null, completed_at: null, terms_version: 'v1'
})
const open = () => mount(QuotaTradesView, { global: { stubs: {
  AppLayout: { template: '<div><slot /></div>' },
  TablePageLayout: { template: '<div><slot name="filters" /><slot name="table" /><slot name="pagination" /></div>' },
  DataTable: { props: ['data'], template: '<div><div v-for="row in data" :key="row.id"><slot name="cell-amount" :row="row" /><slot name="cell-status" :row="row" /><slot name="cell-actions" :row="row" /></div></div>' },
  BaseDialog: { props: ['show'], template: '<div v-if="show"><slot /><slot name="footer" /></div>' },
  Pagination: true
} } })
function dialogConfirm(w: ReturnType<typeof open>) {
  return w.findAll('button').find(button => button.text() === 'common.confirm')!
}
beforeEach(() => {
  vi.clearAllMocks()
  mocks.listQuotaTrades.mockReset().mockResolvedValue({ items: [trade()], total: 1, page: 1, page_size: 20 })
  mocks.getQuotaTradeEvents.mockReset().mockResolvedValue([{ id: 1, action: 'confirm', actor_user_id: 9, actor_name: 'Admin', is_admin: true, created_at: '2026-11-01T00:00:00Z' }])
  for (const action of ['confirmQuotaTrade', 'cancelQuotaTradeReservation', 'withdrawQuotaTrade'] as const) mocks[action].mockReset().mockResolvedValue(trade())
})

describe('administrator quota trades', () => {
  it('shows the recorded CNY listing price in the table and receipt confirmation', async () => {
    mocks.listQuotaTrades.mockResolvedValue({ items: [{ ...trade(), sale_price_cny: 15 }], total: 1 })
    const w = open(); await flushPromises()
    expect(w.text()).toContain('quotaTrades.salePrice ¥15.00')
    await w.get('[data-test="admin-confirm"]').trigger('click')
    expect(w.findAll('p').some(row => row.text() === 'quotaTrades.salePrice ¥15.00')).toBe(true)
  })
  it('lists current and historical statuses and applies all ID filters with paging', async () => {
    const w = open(); await flushPromises()
    expect(mocks.listQuotaTrades).toHaveBeenCalledWith({ page: 1, page_size: 20 }, { signal: expect.any(AbortSignal) })
    await w.get('[data-test="filter-status"]').setValue('expired')
    await w.get('[data-test="filter-group_id"]').setValue('3')
    await w.get('[data-test="filter-account_id"]').setValue('4')
    await w.get('[data-test="filter-user_id"]').setValue('5')
    await w.get('form').trigger('submit'); await flushPromises()
    expect(mocks.listQuotaTrades).toHaveBeenLastCalledWith({ page: 1, page_size: 20, status: 'expired', group_id: 3, account_id: 4, user_id: 5 }, { signal: expect.any(AbortSignal) })
    w.findComponent({ name: 'Pagination' }).vm.$emit('update:page', 2); await flushPromises()
    expect(mocks.listQuotaTrades.mock.calls.at(-1)![0]).toMatchObject({ page: 2, group_id: 3 })
    w.findComponent({ name: 'Pagination' }).vm.$emit('update:pageSize', 50); await flushPromises()
    expect(mocks.listQuotaTrades.mock.calls.at(-1)![0]).toMatchObject({ page: 1, page_size: 50 })
  })

  it('requires a second explicit receipt-verification checkbox for admin completion', async () => {
    const w = open(); await flushPromises()
    expect(w.find('[data-test="admin-withdraw"]').exists()).toBe(false)
    await w.get('[data-test="admin-confirm"]').trigger('click')
    expect(w.text()).toContain('quotaTrades.admin.messages.confirm')
    expect(dialogConfirm(w).attributes('disabled')).toBeDefined()
    await dialogConfirm(w).trigger('click')
    expect(mocks.confirmQuotaTrade).not.toHaveBeenCalled()
    await w.get('[data-test="admin-payment-verified"]').setValue(true)
    await dialogConfirm(w).trigger('click'); await flushPromises()
    expect(mocks.confirmQuotaTrade).toHaveBeenCalledWith(42)
    expect(mocks.listQuotaTrades).toHaveBeenCalledTimes(2)
    expect(mocks.showSuccess).toHaveBeenCalledTimes(1)
  })

  it('cancels reservation only after confirming that the listing reopens and stays frozen', async () => {
    const w = open(); await flushPromises()
    await w.get('[data-test="admin-cancel-reservation"]').trigger('click')
    expect(w.text()).toContain('quotaTrades.admin.messages.cancel-reservation')
    expect(mocks.cancelQuotaTradeReservation).not.toHaveBeenCalled()
    await dialogConfirm(w).trigger('click'); await flushPromises()
    expect(mocks.cancelQuotaTradeReservation).toHaveBeenCalledWith(42)
  })

  it('offers withdrawal only on open listings and preserves terminal history read-only', async () => {
    mocks.listQuotaTrades.mockResolvedValue({ items: [trade('open'), { ...trade('expired'), id: 43 }, { ...trade('completed'), id: 44 }, { ...trade('withdrawn'), id: 45 }], total: 4 })
    const w = open(); await flushPromises()
    expect(w.findAll('[data-test="admin-withdraw"]')).toHaveLength(1)
    expect(w.find('[data-test="admin-confirm"]').exists()).toBe(false)
    expect(w.find('[data-test="admin-cancel-reservation"]').exists()).toBe(false)
    expect(w.findAll('[data-test="trade-events"]')).toHaveLength(4)
    await w.get('[data-test="admin-withdraw"]').trigger('click')
    await dialogConfirm(w).trigger('click'); await flushPromises()
    expect(mocks.withdrawQuotaTrade).toHaveBeenCalledWith(42)
  })

  it('loads actor names and admin event markers and supports retry after a read failure', async () => {
    mocks.getQuotaTradeEvents.mockRejectedValueOnce({ message: 'Temporary event read failure' })
    const w = open(); await flushPromises()
    await w.get('[data-test="trade-events"]').trigger('click'); await flushPromises()
    expect(w.text()).toContain('Temporary event read failure')
    await w.get('[data-test="retry-events"]').trigger('click'); await flushPromises()
    expect(mocks.getQuotaTradeEvents).toHaveBeenLastCalledWith(42, { signal: expect.any(AbortSignal) })
    expect(w.text()).toContain('Admin #9')
    expect(w.text()).toContain('quotaTrades.admin.adminActor')
    expect(w.text()).toContain('quotaTrades.eventActions.confirm')
  })

  it('shows backend mutation errors, does not claim success, and refreshes the listing', async () => {
    mocks.cancelQuotaTradeReservation.mockRejectedValue({ message: 'Cycle has ended' })
    const w = open(); await flushPromises()
    await w.get('[data-test="admin-cancel-reservation"]').trigger('click')
    await dialogConfirm(w).trigger('click'); await flushPromises()
    expect(mocks.showError).toHaveBeenCalledWith('Cycle has ended')
    expect(mocks.showSuccess).not.toHaveBeenCalled()
    expect(mocks.listQuotaTrades).toHaveBeenCalledTimes(2)
  })

  it('deduplicates confirmation while a write is in flight', async () => {
    let finish!: () => void
    mocks.cancelQuotaTradeReservation.mockImplementationOnce(() => new Promise<void>(resolve => { finish = resolve }))
    const w = open(); await flushPromises()
    await w.get('[data-test="admin-cancel-reservation"]').trigger('click')
    await dialogConfirm(w).trigger('click')
    await dialogConfirm(w).trigger('click')
    expect(mocks.cancelQuotaTradeReservation).toHaveBeenCalledTimes(1)
    expect(dialogConfirm(w).attributes('disabled')).toBeDefined()
    finish(); await flushPromises()
  })

  it('retries list failures and rejects invalid ID filters before sending', async () => {
    mocks.listQuotaTrades.mockRejectedValueOnce({ message: 'Temporary list failure' })
    const w = open(); await flushPromises()
    expect(w.get('[role="alert"]').text()).toBe('Temporary list failure')
    await w.get('[data-test="refresh-trades"]').trigger('click'); await flushPromises()
    expect(w.find('[role="alert"]').exists()).toBe(false)
    await w.get('[data-test="filter-group_id"]').setValue('1.5')
    await w.get('form').trigger('submit'); await flushPromises()
    expect(mocks.showError).toHaveBeenCalledWith('quotaTrades.admin.invalidId')
    expect(mocks.listQuotaTrades).toHaveBeenCalledTimes(2)
  })

  it('aborts reads on unmount and ignores late errors', async () => {
    let fail!: (cause: unknown) => void
    mocks.listQuotaTrades.mockImplementationOnce(() => new Promise((_resolve, reject) => { fail = reject }))
    const w = open(); await flushPromises()
    const signal = mocks.listQuotaTrades.mock.calls[0]![1].signal as AbortSignal
    w.unmount()
    expect(signal.aborted).toBe(true)
    fail({ message: 'Aborted' }); await flushPromises()
    expect(mocks.showError).not.toHaveBeenCalled()
  })
})
