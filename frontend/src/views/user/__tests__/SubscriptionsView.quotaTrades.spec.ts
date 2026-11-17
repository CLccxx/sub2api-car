import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { enableAutoUnmount, flushPromises, mount } from '@vue/test-utils'
import SubscriptionsView from '../SubscriptionsView.vue'

const mocks = vi.hoisted(() => ({ getMySubscriptions: vi.fn(), fetchActiveSubscriptions: vi.fn().mockResolvedValue([]), showError: vi.fn() }))
vi.mock('@/api/subscriptions', () => ({ default: { getMySubscriptions: mocks.getMySubscriptions } }))
vi.mock('@/stores/app', () => ({ useAppStore: () => ({ showError: mocks.showError }) }))
vi.mock('@/stores/subscriptions', () => ({ useSubscriptionStore: () => ({ fetchActiveSubscriptions: mocks.fetchActiveSubscriptions }) }))
vi.mock('vue-router', () => ({ useRouter: () => ({ push: vi.fn() }) }))
vi.mock('vue-i18n', async importOriginal => ({ ...await importOriginal<typeof import('vue-i18n')>(), useI18n: () => ({ t: (key: string) => key }) }))
enableAutoUnmount(afterEach)
const subscription = (enabled: boolean | undefined = true) => ({ id: 12, quota_trading_enabled: enabled, status: 'active', expires_at: '2099-01-01T00:00:00Z', weekly_usage_usd: 10, group: { platform: 'openai', weekly_limit_usd: 400 } })
const open = () => mount(SubscriptionsView, { global: { stubs: {
  AppLayout: { template: '<div><slot /></div>' }, Icon: true,
  SharedAccountUsage: { name: 'SharedAccountUsage', template: '<div>upstream unavailable</div>' },
  QuotaTradePanel: { name: 'QuotaTradePanel', props: ['subscriptionId', 'active', 'refreshKey', 'accountEstimate'], template: '<section>trading</section>' }
} } })
beforeEach(() => { vi.clearAllMocks(); mocks.getMySubscriptions.mockResolvedValue([subscription()]) })
describe('subscription trade panel integration', () => {
  it('forwards the shared account estimate to the matching trade panel and clears it when unavailable', async () => {
    const w = open(); await flushPromises()
    const usage = w.findComponent({ name: 'SharedAccountUsage' })
    const panel = w.findComponent({ name: 'QuotaTradePanel' })
    expect(panel.props('accountEstimate')).toBeNull()
    usage.vm.$emit('estimate-updated', 1600)
    await flushPromises()
    expect(panel.props('accountEstimate')).toBe(1600)
    usage.vm.$emit('estimate-updated', null)
    await flushPromises()
    expect(panel.props('accountEstimate')).toBeNull()
    expect(mocks.getMySubscriptions).toHaveBeenCalledTimes(1)
  })
  it('mounts independently of upstream usage availability and updates effective quota after a market read', async () => {
    const w = open(); await flushPromises()
    expect(w.text()).toContain('upstream unavailable')
    const panel = w.findComponent({ name: 'QuotaTradePanel' })
    expect(panel.props('subscriptionId')).toBe(12)
    panel.vm.$emit('quota-updated', { base_limit_usd: 400, adjustment_usd: 0, frozen_usd: 50, used_usd: 10, available_usd: 340 })
    await flushPromises()
    expect(w.text()).toContain('$10.00 / $350.00')
    panel.vm.$emit('quota-updated', { base_limit_usd: 370, adjustment_usd: 0, frozen_usd: 50, used_usd: 10, available_usd: 310 })
    await flushPromises()
    expect(w.text()).toContain('$10.00 / $320.00')
    panel.vm.$emit('quota-updated', { base_limit_usd: 30, adjustment_usd: 0, frozen_usd: 50, used_usd: 10, available_usd: 0 })
    await flushPromises()
    expect(w.text()).toContain('$10.00 / $0.00')
    expect(w.findComponent({ name: 'QuotaTradePanel' }).exists()).toBe(true)
  })
  it.each([true, false])('uses subscription trading capability %s without exposing carpool member settings', async enabled => {
    mocks.getMySubscriptions.mockResolvedValue([subscription(enabled)])
    const w = open(); await flushPromises()
    expect(w.findComponent({ name: 'QuotaTradePanel' }).exists()).toBe(enabled)
  })
  it('keeps the panel hidden for older DTOs without a trading capability', async () => {
    const legacy = subscription()
    delete (legacy as { quota_trading_enabled?: boolean }).quota_trading_enabled
    mocks.getMySubscriptions.mockResolvedValue([legacy])
    const w = open(); await flushPromises()
    expect(w.findComponent({ name: 'QuotaTradePanel' }).exists()).toBe(false)
  })
  it('refreshes subscription and header quota after a mutation without depending on shared quota polling', async () => {
    const w = open(); await flushPromises()
    w.findComponent({ name: 'QuotaTradePanel' }).vm.$emit('changed')
    await flushPromises()
    expect(mocks.getMySubscriptions).toHaveBeenCalledTimes(2)
    expect(mocks.fetchActiveSubscriptions).toHaveBeenCalledWith(true)
  })
  it('keeps zero and negative effective weekly quota visible with safe bounded progress', async () => {
    mocks.getMySubscriptions.mockResolvedValue([{ ...subscription(), effective_weekly_limit_usd: -20 }])
    const w = open(); await flushPromises()
    expect(w.text()).toContain('$10.00 / $-20.00')
    expect(w.text()).not.toContain('userSubscriptions.unlimited')
    expect(w.html()).not.toMatch(/width: (?:-|NaN|Infinity)/)
  })
})
