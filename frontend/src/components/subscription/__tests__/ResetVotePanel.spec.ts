import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { enableAutoUnmount, flushPromises, mount } from '@vue/test-utils'
import type { ResetVoteView } from '@/api/subscriptions'
import ResetVotePanel from '../ResetVotePanel.vue'

const { getResetVote, createResetVote, agreeResetVote } = vi.hoisted(() => ({
  getResetVote: vi.fn(), createResetVote: vi.fn(), agreeResetVote: vi.fn()
}))
vi.mock('@/api/subscriptions', () => ({ default: { getResetVote, createResetVote, agreeResetVote } }))
vi.mock('vue-i18n', async importOriginal => ({
  ...await importOriginal<typeof import('vue-i18n')>(),
  useI18n: () => ({ t: (key: string, params?: unknown) => params ? `${key} ${JSON.stringify(params)}` : key })
}))

const now = new Date('2026-10-08T12:00:00Z')
const idle = (): ResetVoteView => ({ required_count: 4, member_count: 4, can_create: true, blocked_reason: null, proposal: null })
const voting = (changes: Partial<NonNullable<ResetVoteView['proposal']>> = {}): ResetVoteView => ({
  ...idle(), can_create: false,
  proposal: { id: 41, status: 'voting', agreed_count: 1, required_count: 4, my_vote: false, can_agree: true, expires_at: '2026-10-09T12:00:00Z', reason: null, ...changes }
})
const deferred = <T,>() => {
  let resolve!: (value: T) => void
  let reject!: (error: unknown) => void
  const promise = new Promise<T>((yes, no) => { resolve = yes; reject = no })
  return { promise, resolve, reject }
}
const open = (props = {}) => mount(ResetVotePanel, {
  props: { subscriptionId: 12, expiresAt: '2099-01-01T00:00:00Z', resetCredits: 2, usageAvailable: true, ...props },
  global: { stubs: { Icon: true, Teleport: true, Transition: false } }
})
async function showConfirmation(wrapper: ReturnType<typeof open>) {
  await flushPromises()
  await wrapper.get('[data-test="reset-vote-create"]').trigger('click')
}

enableAutoUnmount(afterEach)
beforeEach(() => {
  vi.useFakeTimers({ toFake: ['Date', 'setTimeout', 'clearTimeout', 'setInterval', 'clearInterval'] })
  vi.setSystemTime(now)
  sessionStorage.clear()
  getResetVote.mockReset().mockResolvedValue(idle())
  createResetVote.mockReset().mockResolvedValue(voting({ my_vote: true, can_agree: false }))
  agreeResetVote.mockReset().mockResolvedValue(voting({ agreed_count: 2, my_vote: true, can_agree: false }))
})
afterEach(() => { vi.restoreAllMocks(); vi.useRealTimers() })

describe('ResetVotePanel', () => {
  it.each([null, 0, -1, Number.NaN, Number.POSITIVE_INFINITY])('does not offer creation for %s credits', async resetCredits => {
    const wrapper = open({ resetCredits })
    await flushPromises()
    expect(wrapper.find('[data-test="reset-vote-create"]').exists()).toBe(false)
    expect(createResetVote).not.toHaveBeenCalled()
  })

  it.each([
    { member_count: 3 }, { member_count: 5 }, { required_count: 0 },
    { can_create: false, blocked_reason: 'member_count_mismatch' },
    { blocked_reason: 'source_changed' }
  ])('fails closed for invalid creation eligibility %j', async changes => {
    getResetVote.mockResolvedValue({ ...idle(), ...changes })
    const wrapper = open()
    await flushPromises()
    expect(wrapper.find('[data-test="reset-vote-create"]').exists()).toBe(false)
  })

  it('does not create before status loads or when usage is unavailable', async () => {
    const request = deferred<ResetVoteView>()
    getResetVote.mockReturnValueOnce(request.promise)
    const wrapper = open()
    expect(wrapper.find('[data-test="reset-vote-create"]').exists()).toBe(false)
    request.resolve(idle())
    await flushPromises()
    await wrapper.setProps({ usageAvailable: false })
    expect(wrapper.find('[data-test="reset-vote-create"]').exists()).toBe(false)
  })

  it('requires confirmation of unanimous approval and counts the initiator once', async () => {
    const wrapper = open()
    await showConfirmation(wrapper)
    expect(createResetVote).not.toHaveBeenCalled()
    expect(wrapper.get('[role="dialog"]').text()).toContain('userSubscriptions.resetVote.confirmBody {"required":4}')
    expect(wrapper.get('[role="dialog"]').text()).toContain('userSubscriptions.resetVote.validFor')
    await wrapper.get('[data-test="reset-vote-confirm"]').trigger('click')
    await flushPromises()
    expect(createResetVote).toHaveBeenCalledWith(12, expect.stringMatching(/^[\da-f]{8}-[\da-f]{4}-4[\da-f]{3}-[89ab][\da-f]{3}-[\da-f]{12}$/i), { signal: expect.any(AbortSignal) })
    expect(wrapper.get('[data-test="reset-vote-progress"]').text()).toContain('1/4')
    expect(wrapper.text()).toContain('userSubscriptions.resetVote.myAgreed')
    expect(wrapper.find('[data-test="reset-vote-agree"]').exists()).toBe(false)
    expect(wrapper.find('[role="dialog"]').exists()).toBe(false)
  })

  it('prevents duplicate creates and closes neither by escape nor close controls during submission', async () => {
    const request = deferred<ResetVoteView>()
    createResetVote.mockReturnValueOnce(request.promise)
    const wrapper = open()
    await showConfirmation(wrapper)
    const button = wrapper.get('[data-test="reset-vote-confirm"]')
    await button.trigger('click')
    await button.trigger('click')
    expect(createResetVote).toHaveBeenCalledTimes(1)
    expect(wrapper.get('[data-test="reset-vote-confirm"]').attributes('disabled')).toBeDefined()
    document.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape' }))
    await flushPromises()
    expect(wrapper.find('[role="dialog"]').exists()).toBe(true)
    expect(createResetVote).toHaveBeenCalledTimes(1)
    request.resolve(voting({ my_vote: true, can_agree: false }))
    await flushPromises()
  })

  it('keeps the same UUID after an ambiguous error, closing, and remounting', async () => {
    createResetVote.mockRejectedValueOnce(new Error('secret account=88 card=99 alice@example.test'))
    const wrapper = open()
    await showConfirmation(wrapper)
    await wrapper.get('[data-test="reset-vote-confirm"]').trigger('click')
    await flushPromises()
    const requestId = createResetVote.mock.calls[0]![1]
    expect(wrapper.text()).toContain('userSubscriptions.resetVote.actionFailed')
    expect(wrapper.text()).not.toMatch(/secret|alice|card=99|account=88/)
    await wrapper.get('[data-test="reset-vote-dismiss"]').trigger('click')
    wrapper.unmount()
    const reopened = open()
    await showConfirmation(reopened)
    await reopened.get('[data-test="reset-vote-confirm"]').trigger('click')
    await flushPromises()
    expect(createResetVote.mock.calls[1]![1]).toBe(requestId)
  })

  it('retains its in-memory UUID when storage is unavailable', async () => {
    vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => { throw new Error('disabled') })
    createResetVote.mockRejectedValueOnce(new Error('timeout'))
    const wrapper = open()
    await showConfirmation(wrapper)
    await wrapper.get('[data-test="reset-vote-confirm"]').trigger('click')
    await flushPromises()
    await wrapper.get('[data-test="reset-vote-confirm"]').trigger('click')
    await flushPromises()
    expect(createResetVote.mock.calls[1]![1]).toBe(createResetVote.mock.calls[0]![1])
  })

  it('uses a fresh UUID only for a new request after confirmed success', async () => {
    const wrapper = open()
    await showConfirmation(wrapper)
    await wrapper.get('[data-test="reset-vote-confirm"]').trigger('click')
    await flushPromises()
    getResetVote.mockResolvedValue({ ...voting({ status: 'expired', can_agree: false }), can_create: true })
    await vi.advanceTimersByTimeAsync(10_000)
    await showConfirmation(wrapper)
    await wrapper.get('[data-test="reset-vote-confirm"]').trigger('click')
    await flushPromises()
    expect(createResetVote.mock.calls[1]![1]).not.toBe(createResetVote.mock.calls[0]![1])
  })

  it('reconciles a lost create acknowledgement before starting a new intent', async () => {
    createResetVote.mockRejectedValueOnce(new Error('response lost'))
    const wrapper = open()
    await showConfirmation(wrapper)
    await wrapper.get('[data-test="reset-vote-confirm"]').trigger('click')
    await flushPromises()
    const requestId = createResetVote.mock.calls[0]![1]
    getResetVote.mockResolvedValueOnce({ ...voting({ status: 'completed', my_vote: true }), can_create: true, request_resolved: true })
    await vi.advanceTimersByTimeAsync(10_000)
    expect(getResetVote).toHaveBeenLastCalledWith(12, { signal: expect.any(AbortSignal), requestId })
    expect(sessionStorage.getItem('subscription-reset-vote:12')).toBeNull()
    expect(wrapper.find('[role="dialog"]').exists()).toBe(false)
    expect(wrapper.get('[data-test="reset-vote-create"]').text()).toContain('userSubscriptions.resetVote.create')
    await vi.advanceTimersByTimeAsync(10_000)
    expect(getResetVote).toHaveBeenLastCalledWith(12, { signal: expect.any(AbortSignal) })
    await showConfirmation(wrapper)
    await wrapper.get('[data-test="reset-vote-confirm"]').trigger('click')
    await flushPromises()
    expect(createResetVote.mock.calls[1]![1]).not.toBe(requestId)
  })

  it.each([false, undefined])('keeps an unresolved request explicitly retryable when request_resolved is %s', async requestResolved => {
    createResetVote.mockRejectedValueOnce(new Error('response lost'))
    const wrapper = open()
    await showConfirmation(wrapper)
    await wrapper.get('[data-test="reset-vote-confirm"]').trigger('click')
    await flushPromises()
    const requestId = createResetVote.mock.calls[0]![1]
    await wrapper.get('[data-test="reset-vote-dismiss"]').trigger('click')
    getResetVote.mockResolvedValueOnce({ ...voting({ status: 'completed', my_vote: true }), can_create: true, request_resolved: requestResolved })
    await vi.advanceTimersByTimeAsync(10_000)
    expect(getResetVote).toHaveBeenLastCalledWith(12, { signal: expect.any(AbortSignal), requestId })
    expect(sessionStorage.getItem('subscription-reset-vote:12')).toBe(requestId)
    expect(wrapper.get('[data-test="reset-vote-create"]').text()).toContain('userSubscriptions.resetVote.retry')
    expect(wrapper.text()).toContain('userSubscriptions.resetVote.pendingRequest')
    await wrapper.get('[data-test="reset-vote-create"]').trigger('click')
    const dialog = wrapper.get('[role="dialog"]')
    expect(dialog.text()).toContain('userSubscriptions.resetVote.retryConfirmTitle')
    expect(dialog.text()).toContain('userSubscriptions.resetVote.retryConfirmBody')
    expect(wrapper.get('[data-test="reset-vote-confirm"]').text()).toContain('userSubscriptions.resetVote.retryConfirm')
    await wrapper.get('[data-test="reset-vote-confirm"]').trigger('click')
    await flushPromises()
    expect(createResetVote.mock.calls[1]![1]).toBe(requestId)
    expect(sessionStorage.getItem('subscription-reset-vote:12')).toBeNull()
  })

  it('reconciles restored UUIDs without letting a late old response clear another subscription request', async () => {
    const oldId = 'b8dc84d5-6e30-45d3-b94b-9f4fb6d520aa'
    const newId = '9717e2e7-5d76-40a4-a450-b5d9cd8e5fa8'
    sessionStorage.setItem('subscription-reset-vote:12', oldId)
    sessionStorage.setItem('subscription-reset-vote:13', newId)
    const oldRead = deferred<ResetVoteView>()
    getResetVote.mockReturnValueOnce(oldRead.promise).mockResolvedValue({ ...idle(), request_resolved: false })
    const wrapper = open()
    expect(getResetVote).toHaveBeenLastCalledWith(12, { signal: expect.any(AbortSignal), requestId: oldId })
    await wrapper.setProps({ subscriptionId: 13 })
    await flushPromises()
    oldRead.resolve({ ...idle(), request_resolved: true })
    await flushPromises()
    expect(sessionStorage.getItem('subscription-reset-vote:13')).toBe(newId)
    await showConfirmation(wrapper)
    await wrapper.get('[data-test="reset-vote-confirm"]').trigger('click')
    await flushPromises()
    expect(createResetVote).toHaveBeenCalledWith(13, newId, { signal: expect.any(AbortSignal) })
  })

  it('closes a retry dialog when polling resolves its UUID instead of converting it into a new confirmation', async () => {
    const requestId = 'b8dc84d5-6e30-45d3-b94b-9f4fb6d520aa'
    sessionStorage.setItem('subscription-reset-vote:12', requestId)
    const wrapper = open()
    await showConfirmation(wrapper)
    getResetVote.mockResolvedValueOnce({ ...idle(), request_resolved: true })
    await vi.advanceTimersByTimeAsync(10_000)
    expect(wrapper.find('[role="dialog"]').exists()).toBe(false)
    expect(sessionStorage.getItem('subscription-reset-vote:12')).toBeNull()
    expect(createResetVote).not.toHaveBeenCalled()
  })

  it.each(['no_credit', 'usage_unavailable'])('shows a blocked %s create even alongside a previously completed proposal', async blockedReason => {
    getResetVote.mockResolvedValue({ ...voting({ status: 'completed', my_vote: true }), can_create: true })
    createResetVote.mockResolvedValueOnce({ ...voting({ status: 'completed', my_vote: true }), blocked_reason: blockedReason })
    const wrapper = open()
    await showConfirmation(wrapper)
    await wrapper.get('[data-test="reset-vote-confirm"]').trigger('click')
    await flushPromises()
    expect(wrapper.find('[role="dialog"]').exists()).toBe(false)
    expect(wrapper.text()).toContain('userSubscriptions.resetVote.states.completed')
    expect(wrapper.text()).toContain(`userSubscriptions.resetVote.reasons.${blockedReason}`)
    expect(wrapper.find('[data-test="reset-vote-create"]').exists()).toBe(false)
    await wrapper.setProps({ resetCredits: 0, usageAvailable: false })
    expect(wrapper.text()).toContain(`userSubscriptions.resetVote.reasons.${blockedReason}`)
  })

  it('shows 1/N and the deadline, and submits only one approval while pending', async () => {
    getResetVote.mockResolvedValue(voting())
    const request = deferred<ResetVoteView>()
    agreeResetVote.mockReturnValueOnce(request.promise)
    const wrapper = open()
    await flushPromises()
    expect(wrapper.get('[data-test="reset-vote-progress"]').text()).toContain('1/4')
    expect(wrapper.get('time').attributes('datetime')).toBe('2026-10-09T12:00:00Z')
    const button = wrapper.get('[data-test="reset-vote-agree"]')
    await button.trigger('click')
    await button.trigger('click')
    expect(agreeResetVote).toHaveBeenCalledTimes(1)
    expect(agreeResetVote).toHaveBeenCalledWith(12, 41, { signal: expect.any(AbortSignal) })
    request.resolve(voting({ agreed_count: 2, my_vote: true, can_agree: false }))
    await flushPromises()
    expect(wrapper.get('[data-test="reset-vote-progress"]').text()).toContain('2/4')
    expect(wrapper.text()).toContain('userSubscriptions.resetVote.myAgreed')
  })

  it('does not infer execution from majority or quorum; only the backend controls status', async () => {
    getResetVote.mockResolvedValue(voting({ agreed_count: 3, my_vote: true, can_agree: false }))
    const wrapper = open()
    await flushPromises()
    expect(wrapper.text()).toContain('userSubscriptions.resetVote.states.voting')
    getResetVote.mockResolvedValue(voting({ agreed_count: 4, my_vote: true, can_agree: false }))
    await vi.advanceTimersByTimeAsync(10_000)
    expect(wrapper.text()).toContain('userSubscriptions.resetVote.states.voting')
    expect(createResetVote).not.toHaveBeenCalled()
    expect(agreeResetVote).not.toHaveBeenCalled()
  })

  it.each(['executing', 'awaiting_confirmation', 'applying', 'completed', 'expired', 'invalidated', 'failed'] as const)('renders %s without an obsolete approval action', async status => {
    getResetVote.mockResolvedValue(voting({ status, can_agree: true, reason: 'private account detail' }))
    const wrapper = open()
    await flushPromises()
    expect(wrapper.text()).toContain(`userSubscriptions.resetVote.states.${status}`)
    expect(wrapper.find('[data-test="reset-vote-agree"]').exists()).toBe(false)
    expect(wrapper.find('[data-test="reset-vote-create"]').exists()).toBe(false)
    expect(wrapper.text()).not.toContain('private account detail')
    expect(wrapper.text()).toContain('userSubscriptions.resetVote.reasons.unavailable')
    expect(wrapper.emitted('completed')).toEqual(status === 'completed' ? [[41]] : undefined)
  })

  it.each(['executing', 'awaiting_confirmation', 'applying'] as const)('never creates during %s even with a conflicting can_create flag', async status => {
    getResetVote.mockResolvedValue({ ...voting({ status }), can_create: true })
    const wrapper = open()
    await flushPromises()
    expect(wrapper.find('[data-test="reset-vote-create"]').exists()).toBe(false)
  })

  it.each(['not_configured', 'member_count_mismatch', 'account_unavailable', 'no_credit', 'usage_unavailable', 'reset_in_progress', 'source_changed', 'membership_changed', 'window_reset', 'admin_reset', 'proposal_expired', 'reset_failed', 'confirmation_pending'])('maps safe reason %s', async reason => {
    getResetVote.mockResolvedValue(voting({ status: 'invalidated', reason }))
    const wrapper = open()
    await flushPromises()
    expect(wrapper.text()).toContain(`userSubscriptions.resetVote.reasons.${reason}`)
  })

  it('disables expired voting at the deadline without waiting for the next poll', async () => {
    getResetVote.mockResolvedValue(voting({ expires_at: new Date(now.getTime() + 1000).toISOString() }))
    const wrapper = open()
    await flushPromises()
    await vi.advanceTimersByTimeAsync(1000)
    expect(wrapper.find('[data-test="reset-vote-agree"]').exists()).toBe(false)
    expect(wrapper.text()).toContain('userSubscriptions.resetVote.states.expired')
  })

  it('rechecks the voting deadline when a suspended tab resumes before the clock tick', async () => {
    getResetVote.mockResolvedValue(voting({ expires_at: new Date(now.getTime() + 1000).toISOString() }))
    const wrapper = open()
    await flushPromises()
    vi.setSystemTime(new Date(now.getTime() + 2000))
    await wrapper.get('[data-test="reset-vote-agree"]').trigger('click')
    await flushPromises()
    expect(agreeResetVote).not.toHaveBeenCalled()
    expect(wrapper.text()).toContain('userSubscriptions.resetVote.states.expired')
  })

  it('rechecks subscription expiry before confirming in a resumed tab', async () => {
    const wrapper = open({ expiresAt: new Date(now.getTime() + 1000).toISOString() })
    await showConfirmation(wrapper)
    vi.setSystemTime(new Date(now.getTime() + 2000))
    await wrapper.get('[data-test="reset-vote-confirm"]').trigger('click')
    await flushPromises()
    expect(createResetVote).not.toHaveBeenCalled()
    expect(wrapper.find('[role="dialog"]').exists()).toBe(false)
  })

  it('shows safe unknown statuses without displaying backend text or enabling actions', async () => {
    getResetVote.mockResolvedValue({ ...voting({ status: 'private account detail' as never }), can_create: true })
    const wrapper = open()
    await flushPromises()
    expect(wrapper.text()).toContain('userSubscriptions.resetVote.reasons.unavailable')
    expect(wrapper.text()).not.toContain('private account detail')
    expect(wrapper.find('[data-test="reset-vote-create"]').exists()).toBe(false)
    expect(wrapper.find('[data-test="reset-vote-agree"]').exists()).toBe(false)
  })

  it('retries an uncertain approval without locally incrementing votes or exposing errors', async () => {
    getResetVote.mockResolvedValue(voting())
    agreeResetVote.mockRejectedValueOnce({ message: 'private@example.test', reason: 'private card details' })
    const wrapper = open()
    await flushPromises()
    await wrapper.get('[data-test="reset-vote-agree"]').trigger('click')
    await flushPromises()
    expect(wrapper.text()).toContain('userSubscriptions.resetVote.actionFailed')
    expect(wrapper.text()).not.toContain('private')
    expect(wrapper.get('[data-test="reset-vote-progress"]').text()).toContain('1/4')
    await wrapper.get('[data-test="reset-vote-agree"]').trigger('click')
    await flushPromises()
    expect(agreeResetVote.mock.calls.map(call => call.slice(0, 2))).toEqual([[12, 41], [12, 41]])
    expect(wrapper.get('[data-test="reset-vote-progress"]').text()).toContain('2/4')
  })

  it('clears an uncertain action warning when polling confirms the current vote', async () => {
    getResetVote.mockResolvedValueOnce(voting()).mockResolvedValue(voting({ my_vote: true, can_agree: false, agreed_count: 2 }))
    agreeResetVote.mockRejectedValueOnce(new Error('response lost'))
    const wrapper = open()
    await flushPromises()
    await wrapper.get('[data-test="reset-vote-agree"]').trigger('click')
    await flushPromises()
    expect(wrapper.text()).toContain('userSubscriptions.resetVote.actionFailed')
    await vi.advanceTimersByTimeAsync(10_000)
    expect(wrapper.text()).toContain('userSubscriptions.resetVote.myAgreed')
    expect(wrapper.text()).not.toContain('userSubscriptions.resetVote.actionFailed')
  })

  it('keeps unresolved creation UUIDs scoped to their subscription', async () => {
    createResetVote.mockRejectedValue(new Error('timeout'))
    const wrapper = open()
    await showConfirmation(wrapper)
    await wrapper.get('[data-test="reset-vote-confirm"]').trigger('click')
    await flushPromises()
    await wrapper.setProps({ subscriptionId: 13 })
    await showConfirmation(wrapper)
    await wrapper.get('[data-test="reset-vote-confirm"]').trigger('click')
    await flushPromises()
    await wrapper.setProps({ subscriptionId: 12 })
    await showConfirmation(wrapper)
    await wrapper.get('[data-test="reset-vote-confirm"]').trigger('click')
    await flushPromises()
    const ids = createResetVote.mock.calls.map(call => call[1])
    expect(ids[0]).not.toBe(ids[1])
    expect(ids[2]).toBe(ids[0])
  })

  it('does not treat the voting deadline as permission to leave an uncertain execution', async () => {
    getResetVote.mockResolvedValue(voting({ status: 'awaiting_confirmation', expires_at: new Date(now.getTime() - 1000).toISOString() }))
    const wrapper = open()
    await flushPromises()
    expect(wrapper.text()).toContain('userSubscriptions.resetVote.states.awaiting_confirmation')
    expect(wrapper.text()).not.toContain('userSubscriptions.resetVote.states.expired')
  })

  it('fails closed for unreadable deadlines and mismatched electorate', async () => {
    getResetVote.mockResolvedValue({ ...voting({ expires_at: 'bad date' }), member_count: 3 })
    const wrapper = open()
    await flushPromises()
    expect(wrapper.find('[data-test="reset-vote-agree"]').exists()).toBe(false)
  })

  it('polls status every ten seconds, does not overlap reads, and aborts on unmount', async () => {
    const wrapper = open()
    await flushPromises()
    const request = deferred<ResetVoteView>()
    getResetVote.mockReturnValueOnce(request.promise)
    await vi.advanceTimersByTimeAsync(10_000)
    expect(getResetVote).toHaveBeenCalledTimes(2)
    const signal = getResetVote.mock.calls[1]![1].signal
    await vi.advanceTimersByTimeAsync(30_000)
    expect(getResetVote).toHaveBeenCalledTimes(2)
    wrapper.unmount()
    expect(signal.aborted).toBe(true)
    request.resolve(voting({ status: 'completed' }))
    await flushPromises()
    await vi.advanceTimersByTimeAsync(30_000)
    expect(getResetVote).toHaveBeenCalledTimes(2)
    expect(wrapper.emitted('completed')).toBeUndefined()
  })

  it('disables stale actions on read failure and recovers without leaking backend text', async () => {
    getResetVote.mockResolvedValueOnce(voting()).mockRejectedValueOnce(new Error('secret card identifier'))
    const wrapper = open()
    await flushPromises()
    await vi.advanceTimersByTimeAsync(10_000)
    expect(wrapper.find('[data-test="reset-vote-agree"]').exists()).toBe(false)
    expect(wrapper.text()).toContain('userSubscriptions.resetVote.loadFailed')
    expect(wrapper.text()).not.toContain('secret')
    await vi.advanceTimersByTimeAsync(10_000)
    expect(wrapper.find('[data-test="reset-vote-create"]').exists()).toBe(true)
  })

  it('aborts an old read on subscription changes and ignores its late success', async () => {
    const request = deferred<ResetVoteView>()
    getResetVote.mockReturnValueOnce(request.promise)
    const wrapper = open()
    const signal = getResetVote.mock.calls[0]![1].signal
    await wrapper.setProps({ subscriptionId: 13 })
    await flushPromises()
    expect(signal.aborted).toBe(true)
    request.resolve(voting({ status: 'completed' }))
    await flushPromises()
    expect(wrapper.find('[data-test="reset-vote-progress"]').exists()).toBe(false)
    expect(wrapper.emitted('completed')).toBeUndefined()
  })

  it('aborts a stale read before agreeing and cannot regress the successful mutation', async () => {
    getResetVote.mockResolvedValueOnce(voting())
    const wrapper = open()
    await flushPromises()
    const request = deferred<ResetVoteView>()
    getResetVote.mockReturnValueOnce(request.promise)
    await vi.advanceTimersByTimeAsync(10_000)
    const signal = getResetVote.mock.calls[1]![1].signal
    await wrapper.get('[data-test="reset-vote-agree"]').trigger('click')
    await flushPromises()
    expect(signal.aborted).toBe(true)
    request.resolve(voting())
    await flushPromises()
    expect(wrapper.text()).toContain('userSubscriptions.resetVote.myAgreed')
    expect(wrapper.get('[data-test="reset-vote-progress"]').text()).toContain('2/4')
  })

  it('ignores late mutations after subscription changes', async () => {
    const request = deferred<ResetVoteView>()
    createResetVote.mockReturnValueOnce(request.promise)
    const wrapper = open()
    await showConfirmation(wrapper)
    await wrapper.get('[data-test="reset-vote-confirm"]').trigger('click')
    const signal = createResetVote.mock.calls[0]![2].signal
    await wrapper.setProps({ subscriptionId: 13 })
    await flushPromises()
    expect(signal.aborted).toBe(true)
    request.resolve(voting({ status: 'completed' }))
    await flushPromises()
    expect(wrapper.emitted('completed')).toBeUndefined()
    expect(wrapper.find('[role="dialog"]').exists()).toBe(false)
  })

  it('stops polling and aborts work when the subscription expires', async () => {
    const wrapper = open({ expiresAt: new Date(now.getTime() + 1000).toISOString() })
    await flushPromises()
    const signal = getResetVote.mock.calls[0]![1].signal
    await vi.advanceTimersByTimeAsync(11_000)
    expect(signal.aborted).toBe(true)
    expect(getResetVote).toHaveBeenCalledTimes(1)
    expect(wrapper.find('[data-test="reset-vote-create"]').exists()).toBe(false)
  })

  it('emits completion exactly once both on transition and on the first successful status read', async () => {
    getResetVote.mockResolvedValueOnce(voting({ status: 'applying' })).mockResolvedValue(voting({ status: 'completed' }))
    const wrapper = open()
    await flushPromises()
    await vi.advanceTimersByTimeAsync(30_000)
    expect(wrapper.emitted('completed')).toEqual([[41]])
    wrapper.unmount()
    const reopened = open()
    await flushPromises()
    await vi.advanceTimersByTimeAsync(10_000)
    expect(reopened.emitted('completed')).toEqual([[41]])
  })
})
