import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { enableAutoUnmount, flushPromises, mount } from '@vue/test-utils'
import QuotaTradePanel from '../QuotaTradePanel.vue'
import type { QuotaTrade, SubscriptionQuotaTrades } from '@/types/quotaTrade'

const mocks = vi.hoisted(() => ({
  listQuotaTrades: vi.fn(), createQuotaTrade: vi.fn(), reserveQuotaTrade: vi.fn(), confirmQuotaTrade: vi.fn(), withdrawQuotaTrade: vi.fn(),
  showError: vi.fn(), showSuccess: vi.fn(), user: { id: 1 }
}))
vi.mock('@/api/quotaTrades', () => mocks)
vi.mock('@/stores/auth', () => ({ useAuthStore: () => ({ user: mocks.user }) }))
vi.mock('@/stores/app', () => ({ useAppStore: () => mocks }))
vi.mock('vue-i18n', async importOriginal => ({ ...await importOriginal<typeof import('vue-i18n')>(), useI18n: () => ({ t: (key: string, params?: unknown) => params ? `${key} ${JSON.stringify(params)}` : key }) }))
enableAutoUnmount(afterEach)

const trade = (overrides: Partial<QuotaTrade> = {}): QuotaTrade => ({
  id: 42, group_id: 1, group_name: 'Carpool', account_id: 3, cycle_id: 'cycle-4', seller_user_id: 1, seller_name: 'Seller', seller_subscription_id: 12,
  buyer_user_id: null, buyer_name: null, amount_usd: 50, status: 'open', created_at: '2026-11-01T00:00:00Z', reserved_at: null, completed_at: null, terms_version: 'v1', ...overrides
})
const snapshot = (overrides: Partial<SubscriptionQuotaTrades> = {}): SubscriptionQuotaTrades => ({
  enabled: true, quota: { base_limit_usd: 400, adjustment_usd: 0, frozen_usd: 0, used_usd: 0, available_usd: 400 },
  items: [trade()], total: 1, page: 1, page_size: 20, ...overrides
})
const open = () => mount(QuotaTradePanel, {
  props: { subscriptionId: 12, active: true },
  global: { stubs: { BaseDialog: { template: '<div v-if="show"><slot /><slot name="footer" /></div>', props: ['show'] }, Icon: true, Pagination: true } }
})
async function acceptTerms(w: ReturnType<typeof open>) {
  await w.get('[data-test="trade-terms"]').setValue(true)
}
function dialogConfirm(w: ReturnType<typeof open>) {
  return w.findAll('button').find(button => button.text() === 'common.confirm')!
}
beforeEach(() => {
  vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] })
  vi.clearAllMocks()
  mocks.listQuotaTrades.mockReset().mockResolvedValue(snapshot())
  for (const name of ['createQuotaTrade', 'reserveQuotaTrade', 'confirmQuotaTrade', 'withdrawQuotaTrade'] as const) mocks[name].mockReset().mockResolvedValue(trade())
})
afterEach(() => vi.useRealTimers())

describe('subscriber quota trade panel', () => {
  it('defaults to current listings and keeps each tab’s server-side page independently', async () => {
    const w = open(); await flushPromises()
    expect(w.get('[data-test="trade-tab-current"]').attributes('aria-selected')).toBe('true')
    expect(mocks.listQuotaTrades).toHaveBeenLastCalledWith(12, { page: 1, page_size: 20, view: 'current' }, { signal: expect.any(AbortSignal) })
    w.findComponent({ name: 'Pagination' }).vm.$emit('update:page', 2)
    await flushPromises()
    mocks.listQuotaTrades.mockResolvedValue(snapshot({ items: [trade({ id: 99, status: 'completed' })] }))
    await w.get('[data-test="trade-tab-history"]').trigger('click'); await flushPromises()
    expect(w.get('[data-test="trade-tab-history"]').attributes('aria-selected')).toBe('true')
    expect(mocks.listQuotaTrades).toHaveBeenLastCalledWith(12, { page: 1, page_size: 20, view: 'history' }, { signal: expect.any(AbortSignal) })
    expect(w.find('[data-trade-id="99"]').exists()).toBe(true)
    expect(w.find('[data-trade-id="42"]').exists()).toBe(false)
    for (const action of ['reserve', 'confirm', 'withdraw']) expect(w.find(`[data-test="${action}-trade"]`).exists()).toBe(false)
    w.findComponent({ name: 'Pagination' }).vm.$emit('update:page', 3)
    await flushPromises()
    await w.get('[data-test="trade-tab-current"]').trigger('click'); await flushPromises()
    expect(mocks.listQuotaTrades).toHaveBeenLastCalledWith(12, { page: 2, page_size: 20, view: 'current' }, { signal: expect.any(AbortSignal) })
    await w.get('[data-test="trade-tab-history"]').trigger('click'); await flushPromises()
    expect(mocks.listQuotaTrades).toHaveBeenLastCalledWith(12, { page: 3, page_size: 20, view: 'history' }, { signal: expect.any(AbortSignal) })
  })

  it('does not display previous-tab rows or accept a late response while changing tabs', async () => {
    const w = open(); await flushPromises()
    let finish!: (value: SubscriptionQuotaTrades) => void
    mocks.listQuotaTrades.mockImplementationOnce(() => new Promise(resolve => { finish = resolve }))
    await w.get('[data-test="trade-tab-history"]').trigger('click'); await flushPromises()
    expect(w.find('[data-trade-id="42"]').exists()).toBe(false)
    const historySignal = mocks.listQuotaTrades.mock.calls.at(-1)![2].signal as AbortSignal
    mocks.listQuotaTrades.mockResolvedValue(snapshot())
    await w.get('[data-test="trade-tab-current"]').trigger('click'); await flushPromises()
    expect(historySignal.aborted).toBe(true)
    finish(snapshot({ items: [trade({ id: 99, status: 'completed' })] }))
    await flushPromises()
    expect(w.find('[data-trade-id="99"]').exists()).toBe(false)
    expect(w.find('[data-trade-id="42"]').exists()).toBe(true)
  })

  it('uses a separate empty state for history and never offers historical actions', async () => {
    const w = open(); await flushPromises()
    mocks.listQuotaTrades.mockResolvedValue(snapshot({ items: [], total: 0 }))
    await w.get('[data-test="trade-tab-history"]').trigger('click'); await flushPromises()
    expect(w.text()).toContain('quotaTrades.tabs.historyEmpty')
    expect(w.findComponent({ name: 'Pagination' }).exists()).toBe(false)
    // Even a stale open record returned for historical inspection cannot be acted on.
    mocks.listQuotaTrades.mockResolvedValue(snapshot())
    await w.setProps({ refreshKey: 1 }); await flushPromises()
    expect(w.find('[data-test="withdraw-trade"]').exists()).toBe(false)
  })

  it('does not offer actions when the server marks a listing read-only', async () => {
    mocks.listQuotaTrades.mockResolvedValue(snapshot({ items: [trade({ can_action: false })] }))
    const w = open(); await flushPromises()
    expect(w.find('[data-test="withdraw-trade"]').exists()).toBe(false)
  })

  it('places the current unit price next to the input label and provides an input placeholder', async () => {
    const w = open(); await flushPromises()
    const input = w.get('[data-test="trade-amount"]')
    expect(input.attributes('placeholder')).toBe('quotaTrades.amountPlaceholder')
    const label = w.get('label[for="trade-amount-12"]')
    expect(label.text()).toBe('quotaTrades.amount')
    expect(label.element.nextElementSibling).toBe(w.get('[data-test="suggested-quota-price"]').element)
    expect(input.element.previousElementSibling).toBe(label.element.parentElement)
  })
  it('calculates the suggested unit price from the raw account estimate rather than the personal base', async () => {
    const w = open(); await flushPromises()
    expect(w.get('[data-test="suggested-quota-price"]').text()).toContain('"estimate":1200')
    expect(w.get('[data-test="suggested-quota-price"]').text()).toContain('"price":"0.2917"')
    await w.setProps({ accountEstimate: 1600 })
    expect(w.get('[data-test="suggested-quota-price"]').text()).toContain('"estimate":1600')
    expect(w.get('[data-test="suggested-quota-price"]').text()).toContain('"price":"0.2188"')
    await w.setProps({ accountEstimate: 1400 })
    expect(w.get('[data-test="suggested-quota-price"]').text()).toContain('"price":"0.2500"')
    expect(mocks.listQuotaTrades).toHaveBeenCalledTimes(1)
    for (const value of [null, 0, -1, NaN, Infinity]) {
      await w.setProps({ accountEstimate: value })
      expect(w.get('[data-test="suggested-quota-price"]').text()).toContain('"estimate":1200')
      expect(w.get('[data-test="suggested-quota-price"]').text()).toContain('"price":"0.2917"')
    }
  })
  it('requires one acknowledgement and strictly available > amount, with no extra 10 USD reserve', async () => {
    const w = open(); await flushPromises()
    expect(w.findAll('input[type="checkbox"]')).toHaveLength(1)
    await w.get('[data-test="trade-amount"]').setValue('395')
    expect(w.get('[data-test="create-trade"]').attributes('disabled')).toBeDefined()
    await acceptTerms(w)
    expect(w.get('[data-test="create-trade"]').attributes('disabled')).toBeUndefined()
    for (const value of ['400', '401', '0', '-1', '']) {
      await w.get('[data-test="trade-amount"]').setValue(value)
      expect(w.get('[data-test="create-trade"]').attributes('disabled')).toBeDefined()
    }
    await w.get('[data-test="trade-amount"]').setValue('395')
    await w.get('form').trigger('submit'); await flushPromises()
    expect(mocks.createQuotaTrade).not.toHaveBeenCalled()
    expect(w.text()).toContain('quotaTrades.createConfirmation.message')
    expect(dialogConfirm(w).attributes('disabled')).toBeDefined()
    await w.get('[data-test="trade-sale-price-input"]').setValue('115')
    await dialogConfirm(w).trigger('click'); await flushPromises()
    expect(mocks.createQuotaTrade).toHaveBeenCalledWith(12, { amount_usd: 395, sale_price_cny: 115, accepted_terms: true, request_id: expect.stringMatching(/^[0-9a-f-]{36}$/) })
    expect(w.emitted('changed')).toHaveLength(1)
    expect(mocks.listQuotaTrades).toHaveBeenCalledTimes(2)
  })

  it('retains the UUID on an ambiguous create failure, then retries without a second freeze', async () => {
    mocks.createQuotaTrade.mockRejectedValueOnce({ message: 'Network unavailable' })
    const w = open(); await flushPromises()
    await w.get('[data-test="trade-amount"]').setValue('50'); await acceptTerms(w)
    await w.get('form').trigger('submit'); await flushPromises()
    await w.get('[data-test="trade-sale-price-input"]').setValue('15')
    await dialogConfirm(w).trigger('click'); await flushPromises()
    expect(mocks.showError).toHaveBeenCalledWith('Network unavailable')
    await dialogConfirm(w).trigger('click'); await flushPromises()
    expect(mocks.createQuotaTrade.mock.calls[0]![1]).toEqual(mocks.createQuotaTrade.mock.calls[1]![1])
  })

  it('previews the sale price and allows cancelling a listing without freezing any quota', async () => {
    const w = open(); await flushPromises()
    await w.get('[data-test="trade-amount"]').setValue('50'); await acceptTerms(w)
    await w.get('form').trigger('submit'); await flushPromises()
    expect(mocks.createQuotaTrade).not.toHaveBeenCalled()
    expect(w.get('[data-test="create-unit-price"]').text()).toBe('0.2917')
    expect(w.get('[data-test="create-total-price"]').text()).toBe('0.2917 × 50.00 = 14.59')
    expect(w.text()).toContain('quotaTrades.createConfirmation.priceHint')
    const cancel = w.findAll('button').find(button => button.text() === 'common.cancel')!
    await cancel.trigger('click'); await flushPromises()
    expect(w.find('[data-test="create-price-summary"]').exists()).toBe(false)
    expect(mocks.createQuotaTrade).not.toHaveBeenCalled()
    expect((w.get('[data-test="trade-amount"]').element as HTMLInputElement).value).toBe('50')
  })

  it('submits only the amount reviewed in the dialog and keeps the reference quote stable', async () => {
    const w = open(); await flushPromises()
    await w.setProps({ accountEstimate: 1400 })
    await w.get('[data-test="trade-amount"]').setValue('50'); await acceptTerms(w)
    await w.get('form').trigger('submit'); await flushPromises()
    expect(w.get('[data-test="create-total-price"]').text()).toBe('0.2500 × 50.00 = 12.50')
    await w.get('[data-test="trade-amount"]').setValue('60')
    await w.setProps({ accountEstimate: 1600 })
    expect(w.get('[data-test="create-total-price"]').text()).toBe('0.2500 × 50.00 = 12.50')
    await w.get('[data-test="trade-sale-price-input"]').setValue('12.50')
    await dialogConfirm(w).trigger('click'); await flushPromises()
    expect(mocks.createQuotaTrade).toHaveBeenCalledWith(12, { amount_usd: 50, sale_price_cny: 12.5, accepted_terms: true, request_id: expect.any(String) })
  })

  it('rechecks available quota before the listing dialog can be confirmed', async () => {
    const w = open(); await flushPromises()
    await w.get('[data-test="trade-amount"]').setValue('50'); await acceptTerms(w)
    await w.get('form').trigger('submit'); await flushPromises()
    await w.get('[data-test="trade-sale-price-input"]').setValue('15')
    mocks.listQuotaTrades.mockResolvedValue(snapshot({ quota: { base_limit_usd: 50, adjustment_usd: 0, frozen_usd: 0, used_usd: 0, available_usd: 50 } }))
    await w.setProps({ refreshKey: 1 }); await flushPromises()
    expect(dialogConfirm(w).attributes('disabled')).toBeDefined()
    await dialogConfirm(w).trigger('click'); await flushPromises()
    expect(mocks.createQuotaTrade).not.toHaveBeenCalled()
    await w.setProps({ subscriptionId: 13 }); await flushPromises()
    expect(w.find('[data-test="create-price-summary"]').exists()).toBe(false)
  })

  it('requires a manually entered positive two-decimal sale price before confirmation', async () => {
    const w = open(); await flushPromises()
    await w.get('[data-test="trade-amount"]').setValue('50'); await acceptTerms(w)
    await w.get('form').trigger('submit'); await flushPromises()
    const price = w.get('[data-test="trade-sale-price-input"]')
    expect((price.element as HTMLInputElement).value).toBe('')
    expect(dialogConfirm(w).attributes('disabled')).toBeDefined()
    for (const value of ['', '0', '-1', '1.001', '10000000000.01']) {
      await price.setValue(value)
      expect(dialogConfirm(w).attributes('disabled')).toBeDefined()
      await dialogConfirm(w).trigger('click'); await flushPromises()
      expect(mocks.createQuotaTrade).not.toHaveBeenCalled()
    }
    await price.setValue('14.59')
    expect(dialogConfirm(w).attributes('disabled')).toBeUndefined()
    await dialogConfirm(w).trigger('click'); await flushPromises()
    expect(mocks.createQuotaTrade).toHaveBeenCalledWith(12, { amount_usd: 50, sale_price_cny: 14.59, accepted_terms: true, request_id: expect.any(String) })
  })

  it('uses a different request key if the seller changes the price after an ambiguous failure', async () => {
    mocks.createQuotaTrade.mockRejectedValueOnce({ message: 'Network unavailable' })
    const w = open(); await flushPromises()
    await w.get('[data-test="trade-amount"]').setValue('50'); await acceptTerms(w)
    await w.get('form').trigger('submit'); await flushPromises()
    await w.get('[data-test="trade-sale-price-input"]').setValue('15')
    await dialogConfirm(w).trigger('click'); await flushPromises()
    await w.get('[data-test="trade-sale-price-input"]').setValue('16')
    await dialogConfirm(w).trigger('click'); await flushPromises()
    expect(mocks.createQuotaTrade.mock.calls[0]![1].request_id).not.toBe(mocks.createQuotaTrade.mock.calls[1]![1].request_id)
    expect(mocks.createQuotaTrade.mock.calls[1]![1].sale_price_cny).toBe(16)
  })

  it('shows the recorded sale price after service quota but never invents a price for older listings', async () => {
    mocks.listQuotaTrades.mockResolvedValue(snapshot({ items: [trade({ sale_price_cny: 15 }), trade({ id: 43, sale_price_cny: null })] }))
    const w = open(); await flushPromises()
    expect(w.get('[data-trade-id="42"] [data-test="trade-sale-price"]').text()).toBe('quotaTrades.salePrice ¥15.00')
    expect(w.find('[data-trade-id="43"] [data-test="trade-sale-price"]').exists()).toBe(false)
  })

  it('blocks self-purchase and withdraws only an open own listing after confirmation', async () => {
    const w = open(); await flushPromises()
    expect(w.find('[data-test="reserve-trade"]').exists()).toBe(false)
    await w.get('[data-test="withdraw-trade"]').trigger('click')
    expect(mocks.withdrawQuotaTrade).not.toHaveBeenCalled()
    await dialogConfirm(w).trigger('click'); await flushPromises()
    expect(mocks.withdrawQuotaTrade).toHaveBeenCalledWith(12, 42)
    expect(w.emitted('changed')).toHaveLength(1)
  })

  it('reserves another seller’s open listing only after acknowledging the confirmation', async () => {
    mocks.listQuotaTrades.mockResolvedValue(snapshot({ items: [trade({ seller_user_id: 2, sale_price_cny: 15 })] }))
    const w = open(); await flushPromises()
    expect(w.find('[data-test="withdraw-trade"]').exists()).toBe(false)
    await w.get('[data-test="reserve-trade"]').trigger('click')
    expect(w.text()).toContain('quotaTrades.messages.reserve')
    expect(w.get('[data-test="action-sale-price"]').text()).toBe('quotaTrades.salePrice ¥15.00')
    await dialogConfirm(w).trigger('click'); await flushPromises()
    expect(mocks.reserveQuotaTrade).toHaveBeenCalledWith(12, 42)
  })

  it('requires actual receipt acknowledgement to confirm a reserved sale, even after base quota falls', async () => {
    mocks.listQuotaTrades.mockResolvedValue(snapshot({ items: [trade({ status: 'reserved', buyer_user_id: 2 })], quota: { base_limit_usd: 30, adjustment_usd: 0, frozen_usd: 50, used_usd: 0, available_usd: -20 } }))
    const w = open(); await flushPromises()
    expect(w.find('[data-test="withdraw-trade"]').exists()).toBe(false)
    await w.get('[data-test="confirm-trade"]').trigger('click')
    expect(dialogConfirm(w).attributes('disabled')).toBeDefined()
    await dialogConfirm(w).trigger('click')
    expect(mocks.confirmQuotaTrade).not.toHaveBeenCalled()
    await w.get('[data-test="payment-received"]').setValue(true)
    await dialogConfirm(w).trigger('click'); await flushPromises()
    expect(mocks.confirmQuotaTrade).toHaveBeenCalledWith(12, 42)
  })

  it('offers neither cancellation nor timeout actions to the reserved buyer or historical records', async () => {
    mocks.listQuotaTrades.mockResolvedValue(snapshot({ items: [trade({ seller_user_id: 2, buyer_user_id: 1, status: 'reserved' }), trade({ id: 43, status: 'completed' }), trade({ id: 44, status: 'expired' }), trade({ id: 45, status: 'withdrawn' })] }))
    const w = open(); await flushPromises()
    for (const action of ['reserve', 'confirm', 'withdraw']) expect(w.find(`[data-test="${action}-trade"]`).exists()).toBe(false)
    expect(w.text()).toContain('quotaTrades.status.reserved')
    expect(w.text()).toContain('quotaTrades.status.expired')
  })

  it('keeps the last snapshot on read failure and polls independently, then cancels on unmount', async () => {
    const w = open(); await flushPromises()
    mocks.listQuotaTrades.mockRejectedValueOnce({ message: 'Quota read unavailable' })
    await vi.advanceTimersByTimeAsync(30_000); await flushPromises()
    expect(w.text()).toContain('$400.00')
    expect(w.get('[role="alert"]').text()).toContain('Quota read unavailable')
    expect(w.get('[data-test="create-trade"]').attributes('disabled')).toBeDefined()
    await vi.advanceTimersByTimeAsync(30_000); await flushPromises()
    expect(w.find('[role="alert"]').exists()).toBe(false)
    const signal = mocks.listQuotaTrades.mock.calls.at(-1)![2].signal as AbortSignal
    w.unmount()
    expect(signal.aborted).toBe(true)
    await vi.advanceTimersByTimeAsync(60_000)
    expect(mocks.listQuotaTrades).toHaveBeenCalledTimes(3)
  })

  it('reports mutation errors without treating them as success and refreshes authoritative state', async () => {
    mocks.withdrawQuotaTrade.mockRejectedValue({ message: 'Already reserved' })
    const w = open(); await flushPromises()
    await w.get('[data-test="withdraw-trade"]').trigger('click')
    await dialogConfirm(w).trigger('click'); await flushPromises()
    expect(mocks.showError).toHaveBeenCalledWith('Already reserved')
    expect(w.emitted('changed')).toBeUndefined()
    expect(mocks.listQuotaTrades).toHaveBeenCalledTimes(2)
  })

  it('does not offer active actions for an expired subscription or a disabled market', async () => {
    const w = open(); await flushPromises()
    await w.setProps({ active: false })
    expect(w.find('form').exists()).toBe(false)
    expect(w.get('[data-test="withdraw-trade"]').attributes('disabled')).toBeDefined()
    mocks.listQuotaTrades.mockResolvedValue(snapshot({ enabled: false }))
    await w.setProps({ active: true, refreshKey: 1 }); await flushPromises()
    expect(w.find('form').exists()).toBe(false)
    expect(w.text()).toContain('quotaTrades.disabled')
  })

  it('ignores superseded market reads and late failures on unmount', async () => {
    let finish!: (value: SubscriptionQuotaTrades) => void
    mocks.listQuotaTrades.mockImplementationOnce(() => new Promise(resolve => { finish = resolve }))
    const w = open(); await flushPromises()
    const firstSignal = mocks.listQuotaTrades.mock.calls[0]![2].signal as AbortSignal
    await w.setProps({ refreshKey: 1 }); await flushPromises()
    expect(firstSignal.aborted).toBe(true)
    finish(snapshot({ quota: { base_limit_usd: 999, adjustment_usd: 0, frozen_usd: 0, used_usd: 0, available_usd: 999 } }))
    await flushPromises()
    expect(w.text()).not.toContain('$999.00')
    let fail!: (cause: unknown) => void
    mocks.listQuotaTrades.mockImplementationOnce(() => new Promise((_resolve, reject) => { fail = reject }))
    await w.setProps({ refreshKey: 2 }); await flushPromises()
    w.unmount()
    fail({ message: 'Cancelled' }); await flushPromises()
    expect(mocks.showError).not.toHaveBeenCalled()
  })

  it('groups seller and buyer details into columns with completion time below the listing time, without internal cycle IDs', async () => {
    mocks.listQuotaTrades.mockResolvedValue(snapshot({ items: [trade({ status: 'completed', buyer_user_id: 2, buyer_name: 'Buyer', reserved_at: '2026-11-01T01:00:00Z', completed_at: '2026-11-01T02:00:00Z' })] }))
    const w = open(); await flushPromises()
    const seller = w.get('[data-test="trade-seller-details"]')
    const buyer = w.get('[data-test="trade-buyer-details"]')
    expect(seller.text()).toContain('Seller')
    expect(seller.text()).toContain('quotaTrades.createdAt')
    expect(seller.text()).not.toContain('quotaTrades.reservedAt')
    expect(buyer.text()).toContain('Buyer')
    expect(buyer.classes()).toContain('h-9')
    expect(buyer.classes()).toContain('self-start')
    expect(buyer.text()).toContain('quotaTrades.reservedAt')
    expect(buyer.text()).not.toContain('quotaTrades.createdAt')
    const completed = w.get('[data-test="trade-completed-at"]')
    expect(completed.text()).toContain('quotaTrades.completedAt')
    expect(seller.get('[data-test="trade-completed-at"]').element).toBe(completed.element)
    expect(seller.findAll('p').map(row => row.text())).toEqual([
      expect.stringContaining('quotaTrades.seller'),
      expect.stringContaining('quotaTrades.createdAt'),
      expect.stringContaining('quotaTrades.completedAt')
    ])
    expect(buyer.text()).not.toContain('quotaTrades.completedAt')
    expect(w.text()).not.toContain('cycle-4')
    expect(w.text()).not.toContain('quotaTrades.admin.cycle')
  })

  it('shows buyer placeholders for an open listing without a misleading completion time', async () => {
    const w = open(); await flushPromises()
    expect(w.get('[data-test="trade-buyer-details"]').text()).toContain('quotaTrades.buyer: —')
    expect(w.get('[data-test="trade-buyer-details"]').text()).toContain('quotaTrades.reservedAt: —')
    expect(w.find('[data-test="trade-completed-at"]').exists()).toBe(false)
  })

  it('fetches another page using the subscription-scoped endpoint', async () => {
    const w = open(); await flushPromises()
    w.findComponent({ name: 'Pagination' }).vm.$emit('update:page', 2)
    await flushPromises()
    expect(mocks.listQuotaTrades).toHaveBeenLastCalledWith(12, { page: 2, page_size: 20, view: 'current' }, { signal: expect.any(AbortSignal) })
  })
})
