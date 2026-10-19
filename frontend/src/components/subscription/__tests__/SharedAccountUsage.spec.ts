import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { enableAutoUnmount, flushPromises, mount } from '@vue/test-utils'
import SharedAccountUsage from '../SharedAccountUsage.vue'

const { getSharedAccountUsage, getResetVote, createResetVote, agreeResetVote } = vi.hoisted(() => ({
  getSharedAccountUsage: vi.fn(), getResetVote: vi.fn(), createResetVote: vi.fn(), agreeResetVote: vi.fn()
}))
vi.mock('@/api/subscriptions', () => ({ default: { getSharedAccountUsage, getResetVote, createResetVote, agreeResetVote } }))
vi.mock('vue-i18n', async importOriginal => ({
  ...await importOriginal<typeof import('vue-i18n')>(),
  useI18n: () => ({ t: (key: string, params?: unknown) => params ? `${key} ${JSON.stringify(params)}` : key })
}))

const now = new Date('2026-10-08T12:00:00Z')
const sample = () => ({
  status: 'ready', updated_at: now.toISOString(), refresh_available_at: new Date(now.getTime() + 60_000).toISOString(),
  seven_day: { utilization: 37.5, resets_at: '2026-10-10T18:30:00Z', estimated_total_cost: 250.12 },
  reset_credits: 1, credits: { has_credits: true, unlimited: false, balance: '123.4500' }
})
const open = () => mount(SharedAccountUsage, { props: { subscriptionId: 12, expiresAt: '2099-01-01T00:00:00Z' }, global: { stubs: { Icon: true, HelpTooltip: true } } })
enableAutoUnmount(afterEach)
beforeEach(() => {
  vi.useFakeTimers({ toFake: ['Date', 'setInterval', 'clearInterval', 'setTimeout', 'clearTimeout'] })
  vi.setSystemTime(now)
  getSharedAccountUsage.mockReset().mockResolvedValue(sample())
  getResetVote.mockReset().mockResolvedValue({ required_count: 4, member_count: 4, can_create: false, blocked_reason: 'not_configured', proposal: null })
})
afterEach(() => vi.useRealTimers())

describe('SharedAccountUsage', () => {
  it('offers a confirmed reset request next to available credits without upstream polling', async () => {
    getResetVote.mockResolvedValue({ required_count: 4, member_count: 4, can_create: true, blocked_reason: null, proposal: null })
    const wrapper = open()
    await flushPromises()
    expect(wrapper.find('[data-test="reset-vote-create"]').exists()).toBe(true)
    await vi.advanceTimersByTimeAsync(30_000)
    expect(getSharedAccountUsage).toHaveBeenCalledTimes(1)
    expect(getResetVote).toHaveBeenCalledTimes(4)
  })

  it('refreshes shared usage on a completion refresh key even during the cooldown', async () => {
    const wrapper = open()
    await flushPromises()
    getSharedAccountUsage.mockResolvedValueOnce({ ...sample(), reset_credits: 0 })
    await wrapper.setProps({ refreshKey: 1 })
    await flushPromises()
    expect(getSharedAccountUsage).toHaveBeenCalledTimes(2)
    expect(wrapper.get('[data-test="shared-usage-reset-credits"]').text()).toBe('0')
  })

  it('forwards completed votes and keeps the vote component mounted during usage refresh', async () => {
    const proposal = { id: 41, status: 'applying', agreed_count: 4, required_count: 4, my_vote: true, can_agree: false, expires_at: '2026-10-09T12:00:00Z', reason: null }
    getResetVote.mockResolvedValue({ required_count: 4, member_count: 4, can_create: false, blocked_reason: null, proposal })
    const wrapper = open()
    await flushPromises()
    getResetVote.mockResolvedValueOnce({ required_count: 4, member_count: 4, can_create: false, blocked_reason: null, proposal: { ...proposal, status: 'completed' } })
    await vi.advanceTimersByTimeAsync(10_000)
    expect(wrapper.emitted('reset-completed')).toEqual([[41]])
    const vote = wrapper.findComponent({ name: 'ResetVotePanel' })
    let finish!: (value: ReturnType<typeof sample>) => void
    getSharedAccountUsage.mockImplementationOnce(() => new Promise(resolve => { finish = resolve }))
    await wrapper.setProps({ refreshKey: 1 })
    expect(wrapper.findComponent({ name: 'ResetVotePanel' }).element).toBe(vote.element)
    finish(sample())
    await flushPromises()
  })

  it('shows five read-only account metrics without admin actions', async () => {
    const wrapper = open()
    await flushPromises()
    expect(getSharedAccountUsage).toHaveBeenCalledWith(12, { signal: expect.any(AbortSignal) })
    expect(wrapper.get('[data-test="shared-usage-percent"]').text()).toBe('37.5%')
    expect(wrapper.get('[data-test="shared-usage-estimate"]').text()).toBe('$250.12')
    expect(wrapper.get('[data-test="shared-usage-reset-credits"]').text()).toBe('1')
    expect(wrapper.get('[data-test="shared-usage-points"]').text()).toBe('123.4500')
    expect(wrapper.get('[data-test="shared-usage-countdown"]').text()).toContain('2d 6h 30m')
    expect(wrapper.text()).toContain('userSubscriptions.sharedUsage.updatedAt')
    expect(wrapper.findAll('button')).toHaveLength(1)
    expect(wrapper.findAll('a')).toHaveLength(0)
  })

  it.each(['not_configured', 'multiple_accounts', 'unsupported_account'])('keeps tracking a dispatched vote when usage becomes %s', async status => {
    const proposal = { id: 41, status: 'awaiting_confirmation', agreed_count: 4, required_count: 4, my_vote: true, can_agree: false, expires_at: '2026-10-09T12:00:00Z', reason: null }
    getResetVote.mockResolvedValue({ required_count: 4, member_count: 4, can_create: false, blocked_reason: null, proposal })
    const wrapper = open()
    await flushPromises()
    const voteElement = wrapper.findComponent({ name: 'ResetVotePanel' }).element

    getSharedAccountUsage.mockResolvedValueOnce({ status, updated_at: null, refresh_available_at: null, seven_day: null, reset_credits: null, credits: null })
    await wrapper.setProps({ refreshKey: 1 })
    await flushPromises()

    expect(wrapper.findComponent({ name: 'ResetVotePanel' }).exists()).toBe(true)
    expect(wrapper.findComponent({ name: 'ResetVotePanel' }).element).toBe(voteElement)
    expect(wrapper.find('[data-test="reset-vote-create"]').exists()).toBe(false)
    expect(wrapper.find('[data-test="shared-usage-points"]').exists()).toBe(false)
    getResetVote.mockResolvedValueOnce({ required_count: 4, member_count: 4, can_create: false, blocked_reason: status, proposal: { ...proposal, status: 'completed' } })
    await vi.advanceTimersByTimeAsync(10_000)
    expect(getResetVote).toHaveBeenCalledTimes(2)
    expect(wrapper.emitted('reset-completed')).toEqual([[41]])
  })

  it('throttles refresh and blocks duplicate pending requests', async () => {
    let finish!: (value: ReturnType<typeof sample>) => void
    getSharedAccountUsage.mockImplementationOnce(() => new Promise(resolve => { finish = resolve }))
    const wrapper = open()
    const button = wrapper.get('[data-test="shared-usage-refresh"]')
    expect(button.attributes('disabled')).toBeDefined()
    await button.trigger('click')
    expect(getSharedAccountUsage).toHaveBeenCalledTimes(1)
    finish(sample())
    await flushPromises()
    expect(button.attributes('disabled')).toBeDefined()
    await vi.advanceTimersByTimeAsync(60_000)
    expect(button.attributes('disabled')).toBeUndefined()
    await button.trigger('click')
    await flushPromises()
    expect(getSharedAccountUsage).toHaveBeenCalledTimes(2)
  })

  it('shows unknown metrics rather than fabricated zeros when data is absent', async () => {
    getSharedAccountUsage.mockResolvedValue({ ...sample(), status: 'no_data', seven_day: null, credits: null, reset_credits: null })
    const wrapper = open()
    await flushPromises()
    for (const metric of ['percent', 'estimate', 'reset-credits', 'points', 'countdown']) {
      expect(wrapper.get(`[data-test="shared-usage-${metric}"]`).text()).toBe('userSubscriptions.sharedUsage.unknown')
    }
    expect(wrapper.find('[role="progressbar"]').exists()).toBe(false)
  })

  it.each(['not_configured', 'multiple_accounts', 'unsupported_account', 'unavailable'])('shows a safe %s state', async status => {
    getSharedAccountUsage.mockResolvedValue({ status, updated_at: null, refresh_available_at: null, seven_day: null, reset_credits: null, credits: null })
    const wrapper = open()
    await flushPromises()
    expect(wrapper.text()).toContain(`userSubscriptions.sharedUsage.states.${status}`)
    expect(wrapper.text()).not.toContain('0%')
  })

  it('clears old metrics on an error and does not expose raw server details', async () => {
    const wrapper = open()
    await flushPromises()
    await vi.advanceTimersByTimeAsync(60_000)
    getSharedAccountUsage.mockRejectedValueOnce(new Error('secret@example.test token=private'))
    await wrapper.get('[data-test="shared-usage-refresh"]').trigger('click')
    await flushPromises()
    expect(wrapper.text()).toContain('userSubscriptions.sharedUsage.states.unavailable')
    expect(wrapper.text()).not.toContain('123.4500')
    expect(wrapper.text()).not.toContain('secret')
  })

  it('aborts previous reads and ignores late results when the subscription changes', async () => {
    let finish!: (value: ReturnType<typeof sample>) => void
    getSharedAccountUsage.mockImplementationOnce(() => new Promise(resolve => { finish = resolve }))
    const wrapper = open()
    const oldSignal = getSharedAccountUsage.mock.calls[0]![1].signal
    getSharedAccountUsage.mockResolvedValueOnce({ ...sample(), reset_credits: 4 })
    await wrapper.setProps({ subscriptionId: 13 })
    await flushPromises()
    expect(oldSignal.aborted).toBe(true)
    finish(sample())
    await flushPromises()
    expect(wrapper.get('[data-test="shared-usage-reset-credits"]').text()).toBe('4')
    const signal = getSharedAccountUsage.mock.calls[1]![1].signal
    wrapper.unmount()
    expect(signal.aborted).toBe(true)
  })

  it('removes a rolled-over window instead of claiming it reset to zero', async () => {
    getSharedAccountUsage.mockResolvedValue({ ...sample(), seven_day: { ...sample().seven_day, resets_at: new Date(now.getTime() + 1000).toISOString() } })
    const wrapper = open()
    await flushPromises()
    await vi.advanceTimersByTimeAsync(2000)
    expect(wrapper.get('[data-test="shared-usage-percent"]').text()).toBe('userSubscriptions.sharedUsage.unknown')
    expect(wrapper.get('[data-test="shared-usage-countdown"]').text()).toBe('userSubscriptions.sharedUsage.awaitingRefresh')
  })

  it('clears the shared view and stops requests when the subscription expires', async () => {
    const wrapper = open()
    await flushPromises()
    await wrapper.setProps({ expiresAt: new Date(now.getTime() + 1000).toISOString() })
    await vi.advanceTimersByTimeAsync(2000)
    expect(wrapper.text()).not.toContain('123.4500')
    expect(wrapper.get('[data-test="shared-usage-refresh"]').attributes('disabled')).toBeDefined()
  })

  it('distinguishes zero, unlimited, and available-without-balance credits', async () => {
    const wrapper = open()
    for (const [credits, expected] of [
      [{ has_credits: false, unlimited: false, balance: null }, '0'],
      [{ has_credits: true, unlimited: true, balance: null }, 'userSubscriptions.sharedUsage.unlimited'],
      [{ has_credits: true, unlimited: false, balance: null }, 'userSubscriptions.sharedUsage.available']
    ] as const) {
      getSharedAccountUsage.mockResolvedValueOnce({ ...sample(), credits, refresh_available_at: null })
      await wrapper.setProps({ subscriptionId: wrapper.props('subscriptionId') + 1 })
      await flushPromises()
      expect(wrapper.get('[data-test="shared-usage-points"]').text()).toBe(expected)
    }
  })
})
