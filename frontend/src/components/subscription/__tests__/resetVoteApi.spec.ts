import { beforeEach, describe, expect, it, vi } from 'vitest'
import subscriptionsAPI from '@/api/subscriptions'

const { get, post } = vi.hoisted(() => ({ get: vi.fn(), post: vi.fn() }))
vi.mock('@/api/client', () => ({ apiClient: { get, post } }))

describe('subscriber reset vote API', () => {
  beforeEach(() => vi.resetAllMocks())

  it('reads only subscription-scoped DB status with an abort signal', async () => {
    expect(subscriptionsAPI.getResetVote).toBeTypeOf('function')
    const signal = new AbortController().signal
    const data = { required_count: 4, member_count: 4, can_create: true, blocked_reason: null, proposal: null }
    get.mockResolvedValue({ data })
    expect(await subscriptionsAPI.getResetVote(12, { signal })).toEqual(data)
    expect(get).toHaveBeenCalledWith('/subscriptions/12/reset-vote', { signal })
  })

  it('reconciles a pending UUID through the optional GET query without changing its identity', async () => {
    const signal = new AbortController().signal
    const requestId = 'b8dc84d5-6e30-45d3-b94b-9f4fb6d520aa'
    const data = { request_resolved: true, proposal: { id: 5, status: 'completed' } }
    get.mockResolvedValue({ data })
    expect(await subscriptionsAPI.getResetVote(12, { signal, requestId })).toEqual(data)
    expect(get).toHaveBeenCalledWith('/subscriptions/12/reset-vote', { signal, params: { request_id: requestId } })
  })

  it('forwards cancellation for subscription refreshes', async () => {
    const signal = new AbortController().signal
    get.mockResolvedValue({ data: [] })
    expect(await subscriptionsAPI.getMySubscriptions({ signal })).toEqual([])
    expect(get).toHaveBeenCalledWith('/subscriptions', { signal })
  })

  it('creates using only a stable request UUID and agrees using an empty body', async () => {
    expect(subscriptionsAPI.createResetVote).toBeTypeOf('function')
    expect(subscriptionsAPI.agreeResetVote).toBeTypeOf('function')
    const signal = new AbortController().signal
    const requestId = 'b8dc84d5-6e30-45d3-b94b-9f4fb6d520aa'
    const data = { proposal: { id: 5, status: 'voting' } }
    post.mockResolvedValue({ data })
    expect(await subscriptionsAPI.createResetVote(12, requestId, { signal })).toEqual(data)
    expect(post).toHaveBeenNthCalledWith(1, '/subscriptions/12/reset-vote', { request_id: requestId }, { signal })
    expect(await subscriptionsAPI.agreeResetVote(12, 5, { signal })).toEqual(data)
    expect(post).toHaveBeenNthCalledWith(2, '/subscriptions/12/reset-vote/5/agree', {}, { signal })
  })
})
