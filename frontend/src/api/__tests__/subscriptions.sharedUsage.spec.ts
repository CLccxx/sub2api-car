import { beforeEach, describe, expect, it, vi } from 'vitest'
import subscriptionsAPI from '../subscriptions'

const { get } = vi.hoisted(() => ({ get: vi.fn() }))
vi.mock('../client', () => ({ apiClient: { get } }))

describe('subscriber shared account usage API', () => {
  beforeEach(() => vi.clearAllMocks())
  it('uses only the subscription ID and allows cancelling the read', async () => {
    const signal = new AbortController().signal
    const data = { status: 'no_data', seven_day: null, credits: null, reset_credits: null }
    get.mockResolvedValue({ data })
    expect(await subscriptionsAPI.getSharedAccountUsage(12, { signal })).toEqual(data)
    expect(get).toHaveBeenCalledWith('/subscriptions/12/shared-usage', { signal })
  })
})
