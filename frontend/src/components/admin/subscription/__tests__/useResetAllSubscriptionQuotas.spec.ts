import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises } from '@vue/test-utils'
import type { SubscriptionBulkActionRequest, SubscriptionBulkActionResult } from '@/api/admin/subscriptions'
import { useResetAllSubscriptionQuotas } from '../useResetAllSubscriptionQuotas'

const { list, getResetQuotaTargets, bulkAction } = vi.hoisted(() => ({ list: vi.fn(), getResetQuotaTargets: vi.fn(), bulkAction: vi.fn() }))
vi.mock('@/api/admin', () => ({ adminAPI: { subscriptions: { list, getResetQuotaTargets, bulkAction } } }))

const now = new Date('2026-10-07T12:00:00Z')
let adminId = 4000

function useRows(count: number) {
  getResetQuotaTargets.mockResolvedValue({ subscription_ids: Array.from({ length: count }, (_, index) => index + 1) })
}

function success(request: SubscriptionBulkActionRequest): SubscriptionBulkActionResult {
  return {
    success_count: request.subscription_ids.length, failed_count: 0,
    results: request.subscription_ids.map(subscription_id => ({ subscription_id, success: true }))
  }
}

beforeEach(() => {
  vi.useFakeTimers({ toFake: ['Date'] })
  vi.setSystemTime(now)
  vi.clearAllMocks()
  list.mockReset()
  getResetQuotaTargets.mockReset()
  bulkAction.mockReset()
  localStorage.clear()
  sessionStorage.clear()
  localStorage.setItem('auth_user', JSON.stringify({ id: ++adminId }))
  bulkAction.mockImplementation(async (request: SubscriptionBulkActionRequest) => success(request))
})

afterEach(() => vi.useRealTimers())

describe('global subscription quota reset', () => {
  it('collects a single global ID snapshot without table filters or pagination and never writes before confirmation', async () => {
    useRows(205)
    const operation = useResetAllSubscriptionQuotas()
    await operation.load()
    expect(operation.state.targets).toHaveLength(205)
    expect(list).not.toHaveBeenCalled()
    expect(getResetQuotaTargets).toHaveBeenCalledTimes(1)
    expect(getResetQuotaTargets).toHaveBeenCalledWith({ signal: expect.any(AbortSignal) })
    expect(operation.state.confirmed).toBe(false)
    expect(bulkAction).not.toHaveBeenCalled()
  })

  it('orders the IDs selected by the server', async () => {
    getResetQuotaTargets.mockResolvedValue({ subscription_ids: [2, 1] })
    const operation = useResetAllSubscriptionQuotas()
    await operation.load()
    expect(operation.state.targets.map(row => row.id)).toEqual([1, 2])
  })

  it('does not write when the snapshot request fails', async () => {
    getResetQuotaTargets.mockRejectedValueOnce(new Error('List unavailable'))
    const operation = useResetAllSubscriptionQuotas()
    await operation.load()
    expect(operation.state.targets).toEqual([])
    expect(operation.state.loadError).toBeTruthy()
    await operation.run()
    expect(bulkAction).not.toHaveBeenCalled()
  })

  it('handles no active subscriptions without sending a reset', async () => {
    useRows(0)
    const operation = useResetAllSubscriptionQuotas()
    await operation.load()
    await operation.run()
    expect(operation.counts.value.total).toBe(0)
    expect(bulkAction).not.toHaveBeenCalled()
  })

  it('uses bounded batches and all three windows', async () => {
    useRows(205)
    const operation = useResetAllSubscriptionQuotas()
    await operation.load()
    await operation.run()
    expect(bulkAction.mock.calls.map(([request]) => request.subscription_ids.length)).toEqual([100, 100, 5])
    for (const [request, key] of bulkAction.mock.calls) {
      expect(request).toMatchObject({ action: 'reset_quota', daily: true, weekly: true, monthly: true })
      expect(key).toEqual(expect.any(String))
    }
    expect(operation.counts.value).toEqual({ total: 205, processed: 205, success: 205, failed: 0, skipped: 0 })
    expect(operation.finished.value).toBe(true)
  })

  it('serializes batches and rejects duplicate confirmation clicks', async () => {
    useRows(101)
    let resolve!: (result: SubscriptionBulkActionResult) => void
    bulkAction.mockImplementationOnce(() => new Promise(done => { resolve = done }))
    const operation = useResetAllSubscriptionQuotas()
    await operation.load()
    const first = operation.run()
    await operation.run()
    await flushPromises()
    expect(bulkAction).toHaveBeenCalledTimes(1)
    expect(operation.state.running).toBe(true)
    resolve(success(bulkAction.mock.calls[0]![0]))
    await first
    expect(bulkAction).toHaveBeenCalledTimes(2)
    expect(operation.counts.value.success).toBe(101)
  })

  it('retains partial results and retries only confirmed failures', async () => {
    useRows(3)
    bulkAction.mockResolvedValueOnce({ success_count: 2, failed_count: 1, results: [
      { subscription_id: 1, success: true },
      { subscription_id: 2, success: false, error: 'Temporary failure' },
      { subscription_id: 3, success: true }
    ] })
    const operation = useResetAllSubscriptionQuotas()
    await operation.load()
    await operation.run()
    expect(operation.counts.value).toMatchObject({ success: 2, failed: 1 })
    expect(operation.failedResults.value[0]?.error).toBe('Temporary failure')
    await operation.retryFailed()
    expect(bulkAction.mock.calls[1]![0].subscription_ids).toEqual([2])
    expect(operation.counts.value).toMatchObject({ success: 3, failed: 0, processed: 3 })
  })

  it('retries an uncertain batch with the identical key and never repeats completed batches', async () => {
    useRows(103)
    bulkAction.mockImplementationOnce(async request => success(request))
      .mockRejectedValueOnce({ status: 0, message: 'Connection lost' })
    const operation = useResetAllSubscriptionQuotas()
    await operation.load()
    await operation.run()
    expect(operation.counts.value.success).toBe(100)
    expect(operation.state.pending).not.toBeNull()
    expect(operation.state.requestError).toBe('Connection lost')
    const uncertainCall = bulkAction.mock.calls[1]
    await operation.run()
    expect(bulkAction.mock.calls[2]).toEqual(uncertainCall)
    expect(operation.counts.value.success).toBe(103)
  })

  it('restores an unfinished same-admin operation without recollecting or automatically writing', async () => {
    useRows(103)
    bulkAction.mockImplementationOnce(async request => success(request))
      .mockRejectedValueOnce({ status: 503, message: 'Unavailable' })
    const original = useResetAllSubscriptionQuotas()
    await original.load()
    await original.run()
    original.dispose()
    getResetQuotaTargets.mockClear()
    const restored = useResetAllSubscriptionQuotas()
    await restored.load()
    expect(getResetQuotaTargets).not.toHaveBeenCalled()
    expect(bulkAction).toHaveBeenCalledTimes(2)
    expect(restored.counts.value.success).toBe(100)
    expect(restored.state.resumed).toBe(true)
    await restored.run()
    expect(bulkAction.mock.calls[2]).toEqual(bulkAction.mock.calls[1])
    expect(restored.counts.value.success).toBe(103)
  })

  it('does not restore another administrator\'s checkpoint', async () => {
    useRows(1)
    bulkAction.mockRejectedValueOnce({ status: 0, message: 'Disconnected' })
    const original = useResetAllSubscriptionQuotas()
    await original.load()
    await original.run()
    localStorage.setItem('auth_user', JSON.stringify({ id: ++adminId }))
    const other = useResetAllSubscriptionQuotas()
    await other.load()
    expect(other.state.confirmed).toBe(false)
    expect(other.state.pending).toBeNull()
  })

  it('lets the server recheck expiry so a renewed target is not skipped based on stale data', async () => {
    useRows(1)
    const operation = useResetAllSubscriptionQuotas()
    await operation.load()
    vi.setSystemTime(new Date('2026-10-08T12:00:00Z'))
    await operation.run()
    expect(bulkAction).toHaveBeenCalledTimes(1)
    expect(bulkAction.mock.calls[0]![0].active_only).toBe(true)
    expect(operation.counts.value).toMatchObject({ success: 1, skipped: 0, processed: 1 })
    expect(operation.finished.value).toBe(true)
  })

  it('keeps the guarded uncertain payload so the server can replay or recheck eligibility', async () => {
    useRows(1)
    bulkAction.mockRejectedValueOnce({ status: 0, message: 'Disconnected' })
    const operation = useResetAllSubscriptionQuotas()
    await operation.load()
    await operation.run()
    vi.setSystemTime(new Date('2026-10-07T12:00:02Z'))
    await operation.run()
    expect(bulkAction.mock.calls[1]).toEqual(bulkAction.mock.calls[0])
    expect(bulkAction.mock.calls[0]![0].active_only).toBe(true)
  })

  it('ignores late responses after disposal and recovers using the saved pending key', async () => {
    useRows(101)
    let resolve!: (result: SubscriptionBulkActionResult) => void
    bulkAction.mockImplementationOnce(() => new Promise(done => { resolve = done }))
    const operation = useResetAllSubscriptionQuotas()
    await operation.load()
    const running = operation.run()
    operation.dispose()
    resolve(success(bulkAction.mock.calls[0]![0]))
    await running
    expect(bulkAction).toHaveBeenCalledTimes(1)
    const restored = useResetAllSubscriptionQuotas()
    await restored.load()
    expect(restored.counts.value.success).toBe(0)
    await restored.run()
    expect(bulkAction.mock.calls[1]).toEqual(bulkAction.mock.calls[0])
    expect(bulkAction.mock.calls[2]![0].subscription_ids).toEqual([101])
  })

  it('does not overwrite a newer completed checkpoint with a disposed instance response', async () => {
    useRows(101)
    let resolve!: (result: SubscriptionBulkActionResult) => void
    bulkAction.mockImplementationOnce(() => new Promise(done => { resolve = done }))
    const old = useResetAllSubscriptionQuotas()
    await old.load()
    const running = old.run()
    old.dispose()
    const resumed = useResetAllSubscriptionQuotas()
    await resumed.load()
    await resumed.run()
    expect(resumed.counts.value.success).toBe(101)
    resolve(success(bulkAction.mock.calls[0]![0]))
    await running
    const reloaded = useResetAllSubscriptionQuotas()
    await reloaded.load()
    expect(reloaded.counts.value.success).toBe(101)
    expect(reloaded.finished.value).toBe(true)
  })

  it.each([[0, 1], [1, 1], [1, -2], [1, 1.5]])('rejects an invalid target snapshot %j', async (...ids) => {
    getResetQuotaTargets.mockResolvedValue({ subscription_ids: ids })
    const operation = useResetAllSubscriptionQuotas()
    await operation.load()
    expect(operation.state.loadError).toBeTruthy()
    expect(operation.state.targets).toHaveLength(0)
    await operation.run()
    expect(bulkAction).not.toHaveBeenCalled()
  })

  it('rejects a malformed snapshot instead of reporting an empty operation', async () => {
    getResetQuotaTargets.mockResolvedValue({ total: 3 })
    const operation = useResetAllSubscriptionQuotas()
    await operation.load()
    expect(operation.state.loadError).toBe('admin.subscriptions.resetAll.invalidResponse')
    expect(operation.state.targets).toHaveLength(0)
    expect(bulkAction).not.toHaveBeenCalled()
  })

  it('counts server-side inactive skips separately and never retries them', async () => {
    useRows(2)
    bulkAction.mockResolvedValueOnce({ success_count: 1, failed_count: 0, skipped_count: 1, results: [
      { subscription_id: 1, success: true }, { subscription_id: 2, success: false, skipped: true }
    ] })
    const operation = useResetAllSubscriptionQuotas()
    await operation.load()
    await operation.run()
    expect(operation.counts.value).toMatchObject({ processed: 2, success: 1, failed: 0, skipped: 1 })
    await operation.retryFailed()
    expect(bulkAction).toHaveBeenCalledTimes(1)
  })

  it('blocks uncertain retries after the bounded execution deadline', async () => {
    useRows(1)
    bulkAction.mockRejectedValueOnce({ status: 0, message: 'Connection lost' })
    const operation = useResetAllSubscriptionQuotas()
    await operation.load()
    await operation.run()
    vi.setSystemTime(new Date(now.getTime() + 25 * 60 * 60 * 1000))
    await operation.run()
    expect(bulkAction).toHaveBeenCalledTimes(1)
    expect(operation.state.requestError).toBe('admin.subscriptions.resetAll.replayExpired')
  })

  it('treats a malformed response as uncertain instead of losing pending targets', async () => {
    useRows(2)
    bulkAction.mockResolvedValueOnce({ success_count: 1, failed_count: 0, results: [{ subscription_id: 1, success: true }] })
    const operation = useResetAllSubscriptionQuotas()
    await operation.load()
    await operation.run()
    expect(operation.state.pending).not.toBeNull()
    expect(operation.counts.value.processed).toBe(0)
    await operation.run()
    expect(bulkAction.mock.calls[1]).toEqual(bulkAction.mock.calls[0])
    expect(operation.counts.value.success).toBe(2)
  })

  it('clears an acknowledged completed operation so a future reset has a new key', async () => {
    useRows(1)
    const original = useResetAllSubscriptionQuotas()
    await original.load()
    await original.run()
    original.dismiss()
    const next = useResetAllSubscriptionQuotas()
    await next.load()
    expect(next.state.confirmed).toBe(false)
    await next.run()
    expect(bulkAction.mock.calls[1]![1]).not.toBe(bulkAction.mock.calls[0]![1])
  })
})
