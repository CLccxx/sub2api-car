import { computed, onScopeDispose, ref, watch, type Ref } from 'vue'
import { getUpdateStatus, performUpdate, restartService, type ManagedUpdateStatus } from '@/api/admin/system'

type Dependencies = {
  getUpdateStatus: typeof getUpdateStatus
  performUpdate: typeof performUpdate
  restartService: typeof restartService
  reload: () => void
}

/** The host owns the durable operation; component state only controls polling. */
export function useManagedUpdate(enabled: Ref<boolean>, visible: Ref<boolean>, pageVersion: Ref<string>, dependencies?: Dependencies) {
  const api = dependencies ?? { getUpdateStatus, performUpdate, restartService, reload: () => window.location.reload() }
  const status = ref<ManagedUpdateStatus | null>(null)
  const error = ref('')
  const reconnecting = ref(false)
  const connectionDelayed = ref(false)
  const submitting = ref(false)
  const unresolvedCommand = ref(false)
  const awaitingVersion = ref('')
  const busy = computed(() => submitting.value || unresolvedCommand.value || !!awaitingVersion.value || ['preparing', 'applying'].includes(status.value?.phase ?? ''))
  let timer: ReturnType<typeof setTimeout> | undefined
  let disposed = false
  let polling = false
  let generation = 0
  let reloaded = false
  let availabilityError = false
  let disconnectedSince: number | null = null
  let unresolvedSnapshot: { operation: string; phase: string; since: number } | null = null
  // Capture the first known version of this page, even if later refreshes change the store.
  let loadedVersion = pageVersion.value
  watch(pageVersion, (version) => { if (!loadedVersion && version) loadedVersion = version })

  function clearTimer() {
    if (timer !== undefined) clearTimeout(timer)
    timer = undefined
  }
  function schedule() {
    clearTimer()
    if (!disposed && enabled.value && (visible.value || busy.value) && !reloaded) {
      timer = setTimeout(() => { void refresh() }, connectionDelayed.value ? 5000 : 1500)
    }
  }
  function accept(next: ManagedUpdateStatus) {
    // The Go request can still be checking GitHub before the host sees a job.
    // Wait past its 45s enqueue deadline before interpreting unchanged status.
    const unchanged = unresolvedCommand.value && unresolvedSnapshot !== null &&
      next.operation_id === unresolvedSnapshot.operation && next.phase === unresolvedSnapshot.phase
    if (unchanged && Date.now() - unresolvedSnapshot!.since < 90000) {
      status.value = next
      reconnecting.value = true
      return
    }
    if (unchanged) error.value = 'Update request not confirmed; refresh the status before trying again.'
    unresolvedCommand.value = false
    unresolvedSnapshot = null
    status.value = next
    if (next.phase === 'applying' || (next.phase === 'succeeded' && next.current_version !== next.target_version)) {
      awaitingVersion.value = next.target_version
    }
    if (['failed', 'rolled_back', 'manual_recovery_required', 'ready', 'idle'].includes(next.phase)) {
      awaitingVersion.value = ''
    }
    const pageIsOld = !!loadedVersion && loadedVersion !== next.current_version
    if (next.phase === 'succeeded' && (awaitingVersion.value === next.target_version || pageIsOld) && next.current_version === next.target_version && !reloaded) {
      reloaded = true
      awaitingVersion.value = ''
      clearTimer()
      api.reload()
    }
  }
  function message(cause: unknown) {
    const err = cause as { response?: { data?: { message?: string } }; message?: string }
    return err?.response?.data?.message || err?.message || 'Update service unavailable'
  }

  async function refresh() {
    if (!enabled.value || disposed || polling || submitting.value) return
    clearTimer()
    polling = true
    const requestGeneration = generation
    try {
      const next = await api.getUpdateStatus()
      if (disposed || requestGeneration !== generation || !enabled.value) return
      reconnecting.value = false
      disconnectedSince = null
      connectionDelayed.value = false
      if (availabilityError || ['preparing', 'applying'].includes(next.phase)) error.value = ''
      availabilityError = false
      accept(next)
    } catch (cause) {
      if (disposed || requestGeneration !== generation || !enabled.value) return
      reconnecting.value = busy.value
      if (busy.value) {
        disconnectedSince ??= Date.now()
        connectionDelayed.value = Date.now() - disconnectedSince >= 180000
      }
      if (!busy.value) {
        error.value = message(cause)
        availabilityError = true
      }
    } finally {
      polling = false
      schedule()
    }
  }

  async function command(action: () => ReturnType<typeof api.restartService>) {
    submitting.value = true
    generation++ // Ignore a status response started before this command.
    clearTimer()
    error.value = ''
    const previous = { operation: status.value?.operation_id ?? '', phase: status.value?.phase ?? 'idle', since: Date.now() }
    try {
      const result = await action()
      if (disposed || !enabled.value) return
      if (result.status) accept(result.status)
      else {
        error.value = 'Update request not confirmed; refresh the status before trying again.'
        unresolvedCommand.value = true
        unresolvedSnapshot = previous
      }
    } catch (cause) {
      if (disposed || !enabled.value) return
      error.value = message(cause)
      const failure = cause as { status?: number; response?: { status?: number } }
      const statusCode = failure?.status ?? failure?.response?.status
      unresolvedCommand.value = !statusCode || statusCode >= 500
      unresolvedSnapshot = unresolvedCommand.value ? previous : null
    } finally {
      submitting.value = false
      // A lost response is ambiguous. Ask the host instead of repeating a mutation.
      if (!disposed && enabled.value) {
        if (error.value) await refresh()
        schedule()
      }
    }
  }
  async function prepare() {
    if (!enabled.value || disposed || busy.value || status.value?.phase === 'manual_recovery_required') return
    await command(() => api.performUpdate())
  }
  async function activate() {
    if (!enabled.value || disposed || busy.value || status.value?.phase !== 'ready') return
    const prepared = status.value
    awaitingVersion.value = prepared.target_version
    await command(() => api.restartService(prepared.operation_id))
  }

  watch(enabled, (allowed) => {
    generation++
    clearTimer()
    if (allowed) void refresh()
    else {
      status.value = null
      error.value = ''
      awaitingVersion.value = ''
      unresolvedCommand.value = false
      unresolvedSnapshot = null
      reconnecting.value = false
      disconnectedSince = null
      connectionDelayed.value = false
    }
  }, { immediate: true })
  watch(visible, (open) => { if (open) void refresh(); else schedule() })
  onScopeDispose(() => { disposed = true; generation++; clearTimer() })
  return { status, error, reconnecting, connectionDelayed, submitting, busy, refresh, prepare, activate }
}
