import { computed, reactive } from 'vue'
import { adminAPI } from '@/api/admin'
import type { SubscriptionBulkActionResult } from '@/api/admin/subscriptions'
import {
  completeBulkSubscriptionOperation,
  currentAdminId,
  prepareBulkSubscriptionOperation,
  type BulkSubscriptionOperation
} from './bulkSubscriptionOperation'

interface ResetTarget {
  id: number
}

interface ResetResult {
  subscription_id: number
  success: boolean
  skipped?: boolean
  error?: string
}

interface Checkpoint {
  version: 1
  targets: ResetTarget[]
  results: ResetResult[]
  queue: number[]
  pending: BulkSubscriptionOperation | null
}

const batchSize = 100
const messageKey = (key: string) => `admin.subscriptions.resetAll.${key}`
const validId = (id: number) => Number.isSafeInteger(id) && id > 0

function readCheckpoint(value: string): Checkpoint {
  const saved = JSON.parse(value) as Checkpoint
  if (saved.version !== 1 || !Array.isArray(saved.targets) || !Array.isArray(saved.results) || !Array.isArray(saved.queue)) {
    throw new Error(messageKey('invalidCheckpoint'))
  }
  const ids = new Set(saved.targets.map(target => target.id))
  const resultIds = new Set(saved.results.map(result => result.subscription_id))
  if (ids.size !== saved.targets.length || resultIds.size !== saved.results.length ||
    saved.targets.some(target => !validId(target.id)) ||
    saved.results.some(result => !ids.has(result.subscription_id) || typeof result.success !== 'boolean') ||
    new Set(saved.queue).size !== saved.queue.length || saved.queue.some(id => !ids.has(id)) ||
    saved.results.some(result => (result.success || result.skipped) && saved.queue.includes(result.subscription_id))) {
    throw new Error(messageKey('invalidCheckpoint'))
  }
  if (saved.pending) {
    const { request, key } = saved.pending
    if (typeof key !== 'string' || !key || request?.action !== 'reset_quota' ||
      !request.daily || !request.weekly || !request.monthly || !Array.isArray(request.subscription_ids) ||
      request.active_only !== true || !Number.isFinite(Date.parse(request.execute_before ?? '')) ||
      request.subscription_ids.length === 0 || request.subscription_ids.length > batchSize ||
      new Set(request.subscription_ids).size !== request.subscription_ids.length ||
      request.subscription_ids.some(id => !saved.queue.includes(id))) {
      throw new Error(messageKey('invalidCheckpoint'))
    }
    saved.pending.outcomeUncertain = true
  }
  return saved
}

function responseMatches(result: SubscriptionBulkActionResult, ids: number[]): boolean {
  if (!Array.isArray(result?.results) || result.results.length !== ids.length) return false
  const expected = new Set(ids)
  return result.results.every(item => typeof item.success === 'boolean' && expected.delete(item.subscription_id))
}

export function useResetAllSubscriptionQuotas() {
  const adminId = currentAdminId()
  const storageKey = adminId ? `sub2api:admin:reset-all-quotas:${adminId}` : null
  const state = reactive({
    targets: [] as ResetTarget[],
    results: [] as ResetResult[],
    queue: [] as number[],
    pending: null as BulkSubscriptionOperation | null,
    loading: false,
    loaded: false,
    running: false,
    confirmed: false,
    resumed: false,
    blocked: false,
    recoveryUnavailable: !storageKey,
    loadError: '',
    requestError: ''
  })
  let disposed = false
  let controller: AbortController | null = null

  const counts = computed(() => ({
    total: state.targets.length,
    processed: state.results.length,
    success: state.results.filter(item => item.success).length,
    failed: state.results.filter(item => !item.success && !item.skipped).length,
    skipped: state.results.filter(item => item.skipped).length
  }))
  const failedResults = computed(() => state.results.filter(item => !item.success && !item.skipped))
  const finished = computed(() => state.confirmed && state.queue.length === 0 && !state.pending && !state.running)

  function persist() {
    if (!storageKey || !state.confirmed) return
    const saved: Checkpoint = {
      version: 1, targets: state.targets, results: state.results,
      queue: state.queue, pending: state.pending
    }
    try {
      sessionStorage.setItem(storageKey, JSON.stringify(saved))
    } catch {
      state.recoveryUnavailable = true
    }
  }

  function record(items: ResetResult[]) {
    const results = new Map(state.results.map(item => [item.subscription_id, item]))
    items.forEach(item => results.set(item.subscription_id, item))
    state.results = [...results.values()]
  }

  async function load() {
    if (disposed || state.loading || state.running || state.confirmed) return
    state.loading = true
    state.loaded = false
    state.loadError = ''
    controller = new AbortController()
    const signal = controller.signal
    try {
      let stored: string | null = null
      try {
        stored = storageKey ? sessionStorage.getItem(storageKey) : null
      } catch {
        state.recoveryUnavailable = true
      }
      if (stored) {
        const saved = readCheckpoint(stored)
        Object.assign(state, { targets: saved.targets, results: saved.results, queue: saved.queue, pending: saved.pending })
        state.confirmed = true
        state.resumed = true
        state.loaded = true
        return
      }

      const response = await adminAPI.subscriptions.getResetQuotaTargets({ signal })
      if (disposed || signal.aborted) return
      const ids = response?.subscription_ids
      if (!Array.isArray(ids) || ids.some(id => !validId(id)) || new Set(ids).size !== ids.length) {
        throw new Error(messageKey('invalidResponse'))
      }
      state.targets = ids.map(id => ({ id })).sort((a, b) => a.id - b.id)
      state.loaded = true
    } catch (error) {
      if (disposed || signal.aborted) return
      state.targets = []
      state.loadError = error instanceof Error ? error.message : messageKey('loadFailed')
    } finally {
      state.loading = false
    }
  }

  async function run() {
    if (disposed || state.running || state.loading || !state.loaded || !state.targets.length || finished.value) return
    if (!state.confirmed) {
      state.confirmed = true
      state.queue = state.targets.map(target => target.id)
    }
    state.running = true
    state.requestError = ''
    persist()
    try {
      while (state.queue.length && !disposed) {
        if (currentAdminId() !== adminId) {
          state.requestError = messageKey('adminChanged')
          break
        }
        if (state.pending && Date.parse(state.pending.request.execute_before ?? '') <= Date.now()) {
          state.requestError = messageKey('replayExpired')
          state.blocked = true
          persist()
          break
        }
        if (!state.pending) {
          const ids = state.queue.slice(0, batchSize)
          state.pending = prepareBulkSubscriptionOperation({
            subscription_ids: ids, action: 'reset_quota', daily: true, weekly: true, monthly: true,
            active_only: true, execute_before: new Date(Date.now() + 5 * 60 * 1000).toISOString()
          })
        }

        // Checkpoint the frozen request before sending: reloads and timeouts may
        // happen after the server commits but before the browser sees a response.
        persist()
        const operation = state.pending
        try {
          const result = await adminAPI.subscriptions.bulkAction(operation.request, operation.key)
          if (disposed) return
          if (!responseMatches(result, operation.request.subscription_ids)) throw new Error(messageKey('invalidResponse'))
          record(result.results)
          const completed = new Set(operation.request.subscription_ids)
          state.queue = state.queue.filter(id => !completed.has(id))
          state.pending = null
          persist()
          completeBulkSubscriptionOperation(operation)
        } catch (error: unknown) {
          if (disposed) return
          const failure = error as { status?: number; message?: string; response?: { status?: number; data?: { message?: string } } }
          state.requestError = failure?.message || failure?.response?.data?.message || messageKey('requestFailed')
          const status = failure?.status ?? failure?.response?.status
          if (!operation.outcomeUncertain && status && status >= 400 && status < 500 && status !== 408 && status !== 409) {
            completeBulkSubscriptionOperation(operation)
            state.pending = null
          } else {
            operation.outcomeUncertain = true
          }
          persist()
          break
        }
      }
    } finally {
      state.running = false
    }
  }

  async function retryFailed() {
    if (!finished.value || !failedResults.value.length || disposed) return
    state.queue = failedResults.value.map(item => item.subscription_id)
    state.results = state.results.filter(item => item.success || item.skipped)
    persist()
    await run()
  }

  function dismiss() {
    if (state.running) return
    if ((finished.value || state.blocked) && storageKey) {
      try { sessionStorage.removeItem(storageKey) } catch { /* Recovery is best-effort when storage is unavailable. */ }
    }
  }

  function dispose() {
    disposed = true
    controller?.abort()
  }

  return { state, counts, failedResults, finished, load, run, retryFailed, dismiss, dispose }
}
