import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { enableAutoUnmount, flushPromises, mount } from '@vue/test-utils'
import SubscriptionsView from '../SubscriptionsView.vue'

const { getMySubscriptions, getSharedAccountUsage, getResetVote, showError } = vi.hoisted(() => ({
  getMySubscriptions: vi.fn(), getSharedAccountUsage: vi.fn(), getResetVote: vi.fn(), showError: vi.fn()
}))
vi.mock('@/api/subscriptions', () => ({ default: { getMySubscriptions, getSharedAccountUsage, getResetVote } }))
vi.mock('@/stores/app', () => ({ useAppStore: () => ({ showError }) }))
vi.mock('@/components/layout/AppLayout.vue', () => ({ default: { template: '<div><slot /></div>' } }))
vi.mock('vue-router', () => ({ useRouter: () => ({ push: vi.fn() }) }))
vi.mock('vue-i18n', async importOriginal => ({
  ...await importOriginal<typeof import('vue-i18n')>(),
  useI18n: () => ({ t: (key: string) => key })
}))
enableAutoUnmount(afterEach)
beforeEach(() => {
  vi.useFakeTimers({ toFake: ['Date', 'setTimeout', 'clearTimeout', 'setInterval', 'clearInterval'] })
  getMySubscriptions.mockReset()
  getSharedAccountUsage.mockReset().mockResolvedValue({ status: 'no_data', reset_credits: null, seven_day: null, credits: null, updated_at: null, refresh_available_at: null })
  getResetVote.mockReset()
  showError.mockReset()
})
afterEach(() => vi.useRealTimers())

const subscription = (usage: number) => ({
  id: 1, status: 'active', expires_at: '2099-01-01T00:00:00Z', daily_usage_usd: usage,
  group: { platform: 'openai', daily_limit_usd: 10 }
})
const open = () => mount(SubscriptionsView, { global: { stubs: {
  AppLayout: { template: '<div><slot /></div>' }, Icon: true,
  SharedAccountUsage: { name: 'SharedAccountUsage', props: ['subscriptionId', 'expiresAt', 'refreshKey'], template: '<section />' }
} } })

describe('my subscriptions shared usage', () => {
  it('retries a failed completion refresh and marks completion only after fresh quotas arrive', async () => {
    getMySubscriptions.mockResolvedValueOnce([subscription(9)]).mockRejectedValueOnce(new Error('transient failure')).mockResolvedValue([subscription(0)])
    const wrapper = open()
    await flushPromises()
    const shared = wrapper.findComponent({ name: 'SharedAccountUsage' })
    shared.vm.$emit('reset-completed', 41)
    await flushPromises()
    expect(wrapper.text()).toContain('$9.00 / $10.00')
    expect(showError).toHaveBeenCalledTimes(1)
    shared.vm.$emit('reset-completed', 41)
    await flushPromises()
    await vi.advanceTimersByTimeAsync(999)
    expect(getMySubscriptions).toHaveBeenCalledTimes(2)
    await vi.advanceTimersByTimeAsync(1)
    await flushPromises()
    expect(getMySubscriptions).toHaveBeenCalledTimes(3)
    expect(wrapper.text()).toContain('$0.00 / $10.00')
    shared.vm.$emit('reset-completed', 41)
    await vi.advanceTimersByTimeAsync(120_000)
    expect(getMySubscriptions).toHaveBeenCalledTimes(3)
  })

  it('caps the retry delay without abandoning a pending completion or overlapping requests', async () => {
    getMySubscriptions.mockResolvedValueOnce([subscription(9)]).mockRejectedValue(new Error('temporarily offline'))
    const wrapper = open()
    await flushPromises()
    wrapper.findComponent({ name: 'SharedAccountUsage' }).vm.$emit('reset-completed', 41)
    await flushPromises()
    for (const delay of [1000, 2000, 4000, 8000, 16_000, 30_000, 30_000]) {
      const count = getMySubscriptions.mock.calls.length
      await vi.advanceTimersByTimeAsync(delay - 1)
      expect(getMySubscriptions).toHaveBeenCalledTimes(count)
      await vi.advanceTimersByTimeAsync(1)
      expect(getMySubscriptions).toHaveBeenCalledTimes(count + 1)
    }
    expect(showError).toHaveBeenCalledTimes(1)
    let finish!: (value: unknown) => void
    getMySubscriptions.mockImplementationOnce(() => new Promise(resolve => { finish = resolve }))
    await vi.advanceTimersByTimeAsync(30_000)
    const count = getMySubscriptions.mock.calls.length
    await vi.advanceTimersByTimeAsync(120_000)
    expect(getMySubscriptions).toHaveBeenCalledTimes(count)
    finish([subscription(0)])
    await flushPromises()
    expect(wrapper.text()).toContain('$0.00 / $10.00')
    await vi.advanceTimersByTimeAsync(120_000)
    expect(getMySubscriptions).toHaveBeenCalledTimes(count)
  })

  it('cancels a scheduled completion retry on unmount', async () => {
    getMySubscriptions.mockResolvedValueOnce([subscription(9)]).mockRejectedValue(new Error('transient failure'))
    const wrapper = open()
    await flushPromises()
    wrapper.findComponent({ name: 'SharedAccountUsage' }).vm.$emit('reset-completed', 41)
    await flushPromises()
    wrapper.unmount()
    await vi.advanceTimersByTimeAsync(120_000)
    expect(getMySubscriptions).toHaveBeenCalledTimes(2)
  })

  it('aborts an in-flight completion refresh and ignores its late error on unmount', async () => {
    getMySubscriptions.mockResolvedValueOnce([subscription(9)])
    let fail!: (reason: unknown) => void
    getMySubscriptions.mockImplementationOnce(() => new Promise((_resolve, reject) => { fail = reject }))
    const wrapper = open()
    await flushPromises()
    wrapper.findComponent({ name: 'SharedAccountUsage' }).vm.$emit('reset-completed', 41)
    await flushPromises()
    const signal = getMySubscriptions.mock.calls[1]![0]?.signal as AbortSignal | undefined
    expect(signal).toBeInstanceOf(AbortSignal)
    wrapper.unmount()
    expect(signal?.aborted).toBe(true)
    fail(new Error('request cancelled'))
    await flushPromises()
    await vi.advanceTimersByTimeAsync(120_000)
    expect(getMySubscriptions).toHaveBeenCalledTimes(2)
    expect(showError).not.toHaveBeenCalled()
  })

  it('refreshes again for a different completion arriving during an older quota read', async () => {
    getMySubscriptions.mockResolvedValueOnce([subscription(9)])
    let finish!: (value: unknown) => void
    getMySubscriptions.mockImplementationOnce(() => new Promise(resolve => { finish = resolve })).mockResolvedValue([subscription(0)])
    const wrapper = open()
    await flushPromises()
    const shared = wrapper.findComponent({ name: 'SharedAccountUsage' })
    shared.vm.$emit('reset-completed', 41)
    shared.vm.$emit('reset-completed', 42)
    await flushPromises()
    expect(getMySubscriptions).toHaveBeenCalledTimes(2)
    finish([subscription(3)])
    await flushPromises()
    await vi.advanceTimersByTimeAsync(0)
    await flushPromises()
    expect(getMySubscriptions).toHaveBeenCalledTimes(3)
    expect(wrapper.text()).toContain('$0.00 / $10.00')
    shared.vm.$emit('reset-completed', 42)
    await vi.advanceTimersByTimeAsync(60_000)
    expect(getMySubscriptions).toHaveBeenCalledTimes(3)
  })

  it('refreshes stale initial quotas when the first vote read is already completed without a refresh loop', async () => {
    getMySubscriptions.mockResolvedValueOnce([subscription(9)]).mockResolvedValue([subscription(0)])
    getResetVote.mockResolvedValue({ required_count: 4, member_count: 4, can_create: false, blocked_reason: null, proposal: {
      id: 41, status: 'completed', agreed_count: 4, required_count: 4, my_vote: true, can_agree: false,
      expires_at: '2099-01-01T00:00:00Z', reason: null
    } })
    const wrapper = mount(SubscriptionsView, { global: { stubs: {
      AppLayout: { template: '<div><slot /></div>' }, Icon: true, HelpTooltip: true
    } } })
    await flushPromises()
    expect(getMySubscriptions).toHaveBeenCalledTimes(2)
    expect(wrapper.text()).toContain('$0.00 / $10.00')
    await vi.advanceTimersByTimeAsync(30_000)
    expect(getMySubscriptions).toHaveBeenCalledTimes(2)
    expect(getSharedAccountUsage).toHaveBeenCalledTimes(2)
    expect(getResetVote).toHaveBeenCalledTimes(4)
  })

  it('refreshes all quotas and shared displays once per completed proposal without unmounting panels', async () => {
    const subscription = { id: 1, status: 'active', expires_at: '2099-01-01T00:00:00Z', daily_usage_usd: 2, group: { platform: 'openai', daily_limit_usd: 10 } }
    getMySubscriptions.mockResolvedValue([subscription, { ...subscription, id: 2 }])
    const wrapper = mount(SubscriptionsView, { global: { stubs: {
      AppLayout: { template: '<div><slot /></div>' }, Icon: true,
      SharedAccountUsage: { name: 'SharedAccountUsage', props: ['subscriptionId', 'expiresAt', 'refreshKey'], template: '<section />' }
    } } })
    await flushPromises()
    const sections = wrapper.findAllComponents({ name: 'SharedAccountUsage' })
    let finish!: (value: unknown) => void
    getMySubscriptions.mockImplementationOnce(() => new Promise(resolve => { finish = resolve }))
    sections[0]!.vm.$emit('reset-completed', 41)
    sections[1]!.vm.$emit('reset-completed', 41)
    await flushPromises()
    expect(getMySubscriptions).toHaveBeenCalledTimes(2)
    expect(wrapper.findComponent({ name: 'SharedAccountUsage' }).element).toBe(sections[0]!.element)
    for (const section of sections) expect(section.props('refreshKey')).toBe(1)
    finish([{ ...subscription, daily_usage_usd: 0 }, { ...subscription, id: 2, daily_usage_usd: 0 }])
    await flushPromises()
    expect(wrapper.text()).toContain('$0.00 / $10.00')
    expect(wrapper.text()).not.toContain('$2.00 / $10.00')
  })

  it('keeps individual quotas and mounts shared usage only for active OpenAI subscriptions', async () => {
    getMySubscriptions.mockResolvedValue([
      { id: 1, status: 'active', expires_at: '2099-01-01T00:00:00Z', daily_usage_usd: 2, group: { name: 'My group', platform: 'openai', daily_limit_usd: 10 } },
      { id: 2, status: 'expired', expires_at: '2020-01-01T00:00:00Z', group: { platform: 'openai' } },
      { id: 3, status: 'active', expires_at: '2099-01-01T00:00:00Z', group: { platform: 'anthropic' } },
      { id: 4, status: 'active', expires_at: '2020-01-01T00:00:00Z', group: { platform: 'openai' } }
    ])
    const wrapper = mount(SubscriptionsView, { global: { stubs: {
      AppLayout: { template: '<div><slot /></div>' }, Icon: true,
      SharedAccountUsage: { name: 'SharedAccountUsage', props: ['subscriptionId', 'expiresAt'], template: '<section />' }
    } } })
    await flushPromises()
    const sections = wrapper.findAllComponents({ name: 'SharedAccountUsage' })
    expect(sections).toHaveLength(1)
    expect(sections[0]!.props('subscriptionId')).toBe(1)
    expect(wrapper.text()).toContain('$2.00 / $10.00')
    expect(wrapper.text()).toContain('userSubscriptions.personalQuota')
  })
})
