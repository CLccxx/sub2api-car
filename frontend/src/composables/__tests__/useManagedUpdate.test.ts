import { effectScope, ref, nextTick } from 'vue'
import { describe, it, expect, vi, afterEach } from 'vitest'
import { useManagedUpdate } from '../useManagedUpdate'
import type { ManagedUpdateStatus } from '@/api/admin/system'

function status(phase: ManagedUpdateStatus['phase'], current = '0.2.17-car.6'): ManagedUpdateStatus {
  return { protocol_version: 1, phase, operation_id: phase === 'idle' ? '' : 'sysop-test', target_version: phase === 'idle' ? '' : '0.2.17-car.7', target_commit: 'a'.repeat(40), current_version: current, message: '', error_code: '', backup_path: '' }
}

const scopes: ReturnType<typeof effectScope>[] = []
function fixture(initial = status('idle'), enabled = true) {
  vi.useFakeTimers()
  const api = { getUpdateStatus: vi.fn().mockResolvedValue(initial), performUpdate: vi.fn().mockResolvedValue({ status: status('preparing') }), restartService: vi.fn().mockResolvedValue({ status: status('applying') }) }
  const reload = vi.fn()
  const visible = ref(true)
  const allowed = ref(enabled)
  const scope = effectScope()
  scopes.push(scope)
  const loadedVersion = ref(initial.current_version)
  const update = scope.run(() => useManagedUpdate(allowed, visible, loadedVersion, { ...api, reload }))!
  return { update, api, reload, visible, allowed, scope }
}
async function settle() { await vi.advanceTimersByTimeAsync(0); await nextTick() }
afterEach(() => { for (const scope of scopes.splice(0)) scope.stop(); vi.useRealTimers() })

describe('managed web updates', () => {
  it('prepares without activating and restores ready state from the host', async () => {
    const f = fixture()
    await settle()
    await f.update.prepare()
    expect(f.update.status.value?.phase).toBe('preparing')
    expect(f.api.restartService).not.toHaveBeenCalled()
    f.api.getUpdateStatus.mockResolvedValue(status('ready'))
    await vi.advanceTimersByTimeAsync(1500)
    expect(f.update.status.value?.phase).toBe('ready')
    const reopened = fixture(status('ready'))
    await settle()
    expect(reopened.update.status.value?.phase).toBe('ready')
    expect(reopened.api.performUpdate).not.toHaveBeenCalled()
  })

  it('waits for verified running version even after a long restart', async () => {
    const f = fixture(status('ready'))
    await settle()
    await f.update.activate()
    expect(f.api.restartService).toHaveBeenCalledWith('sysop-test')
    f.api.getUpdateStatus.mockResolvedValue(status('succeeded'))
    await vi.advanceTimersByTimeAsync(30000)
    expect(f.reload).not.toHaveBeenCalled()
    f.api.getUpdateStatus.mockResolvedValue(status('succeeded', '0.2.17-car.7'))
    await vi.advanceTimersByTimeAsync(1500)
    expect(f.reload).toHaveBeenCalledTimes(1)
  })

  it('reconnects after a lost activation response', async () => {
    const f = fixture(status('ready'))
    await settle()
    f.api.restartService.mockRejectedValue(new Error('network lost'))
    f.api.getUpdateStatus.mockRejectedValue(new Error('temporarily offline'))
    await f.update.activate()
    expect(f.reload).not.toHaveBeenCalled()
    f.api.getUpdateStatus.mockResolvedValue(status('applying'))
    await vi.advanceTimersByTimeAsync(1500)
    f.api.getUpdateStatus.mockResolvedValue(status('succeeded', '0.2.17-car.7'))
    await vi.advanceTimersByTimeAsync(1500)
    expect(f.reload).toHaveBeenCalledTimes(1)
  })

  it('keeps an ambiguous preparation busy and polls even while closed', async () => {
    const f = fixture()
    await settle()
    f.api.performUpdate.mockRejectedValue(new Error('lost prepare response'))
    f.api.getUpdateStatus.mockRejectedValue(new Error('offline'))
    await f.update.prepare()
    expect(f.update.busy.value).toBe(true)
    expect(f.update.reconnecting.value).toBe(true)
    await f.update.prepare()
    expect(f.api.performUpdate).toHaveBeenCalledTimes(1)
    f.visible.value = false
    await nextTick()
    const polls = f.api.getUpdateStatus.mock.calls.length
    await vi.advanceTimersByTimeAsync(1500)
    expect(f.api.getUpdateStatus.mock.calls.length).toBeGreaterThan(polls)
    f.api.getUpdateStatus.mockResolvedValue(status('ready'))
    await vi.advanceTimersByTimeAsync(1500)
    expect(f.update.busy.value).toBe(false)
    expect(f.update.status.value?.phase).toBe('ready')
  })

  it('reloads an old tab that missed activation in another tab', async () => {
    const f = fixture()
    await settle()
    f.visible.value = false
    await nextTick()
    f.api.getUpdateStatus.mockResolvedValue(status('succeeded', '0.2.17-car.7'))
    f.visible.value = true
    await settle()
    expect(f.reload).toHaveBeenCalledTimes(1)
  })

  it('continues waiting through idle while a disconnected prepare is still being accepted', async () => {
    const f = fixture()
    await settle()
    f.api.performUpdate.mockRejectedValue(new Error('early disconnect'))
    await f.update.prepare()
    f.visible.value = false
    await nextTick()
    await vi.advanceTimersByTimeAsync(15000)
    expect(f.update.busy.value).toBe(true)
    await f.update.prepare()
    expect(f.api.performUpdate).toHaveBeenCalledTimes(1)
    f.api.getUpdateStatus.mockResolvedValue(status('preparing'))
    await vi.advanceTimersByTimeAsync(1500)
    expect(f.update.status.value?.phase).toBe('preparing')
    f.api.getUpdateStatus.mockResolvedValue(status('ready'))
    await vi.advanceTimersByTimeAsync(1500)
    expect(f.update.busy.value).toBe(false)
  })

  it('resolves unchanged idle only after the bounded enqueue deadline', async () => {
    const f = fixture()
    await settle()
    f.api.performUpdate.mockRejectedValue(new Error('early disconnect'))
    await f.update.prepare()
    await vi.advanceTimersByTimeAsync(45000)
    expect(f.update.busy.value).toBe(true)
    await vi.advanceTimersByTimeAsync(46500)
    expect(f.update.busy.value).toBe(false)
    expect(f.update.error.value).toContain('not confirmed')
    expect(f.api.performUpdate).toHaveBeenCalledTimes(1)
  })

  it('treats a normalized HTTP conflict as a definite rejection', async () => {
    const f = fixture()
    await settle()
    f.api.performUpdate.mockRejectedValue({ status: 409, message: 'Another update is active' })
    await f.update.prepare()
    expect(f.update.busy.value).toBe(false)
    expect(f.update.error.value).toBe('Another update is active')
  })

  it('keeps normalized status zero ambiguous', async () => {
    const f = fixture()
    await settle()
    f.api.performUpdate.mockRejectedValue({ status: 0, message: 'Network unavailable' })
    await f.update.prepare()
    expect(f.update.busy.value).toBe(true)
  })

  it.each(['failed', 'rolled_back', 'manual_recovery_required'] as const)('shows %s without reloading', async (phase) => {
    const f = fixture(status('applying'))
    await settle()
    f.api.getUpdateStatus.mockResolvedValue(status(phase))
    await vi.advanceTimersByTimeAsync(1500)
    expect(f.update.status.value?.phase).toBe(phase)
    expect(f.reload).not.toHaveBeenCalled()
    expect(f.update.busy.value).toBe(false)
  })

  it('does not reload on every mount after a completed update', async () => {
    const f = fixture(status('succeeded', '0.2.17-car.7'))
    await settle()
    await vi.advanceTimersByTimeAsync(5000)
    expect(f.reload).not.toHaveBeenCalled()
  })

  it('clears a service availability error after reconnecting', async () => {
    const f = fixture()
    await settle()
    f.api.getUpdateStatus.mockRejectedValueOnce(new Error('updater unavailable'))
    await f.update.refresh()
    expect(f.update.error.value).toBe('updater unavailable')
    await f.update.refresh()
    expect(f.update.error.value).toBe('')
  })

  it('shows a server-check hint after prolonged disconnection without claiming success', async () => {
    const f = fixture(status('applying'))
    await settle()
    f.api.getUpdateStatus.mockRejectedValue(new Error('offline'))
    await vi.advanceTimersByTimeAsync(185000)
    expect(f.update.connectionDelayed.value).toBe(true)
    expect(f.update.busy.value).toBe(true)
    expect(f.reload).not.toHaveBeenCalled()
    f.api.getUpdateStatus.mockResolvedValue(status('rolled_back'))
    await vi.advanceTimersByTimeAsync(5000)
    expect(f.update.connectionDelayed.value).toBe(false)
    expect(f.update.status.value?.phase).toBe('rolled_back')
  })

  it('never queries administrator endpoints for a normal user', async () => {
    const f = fixture(status('idle'), false)
    await settle()
    await f.update.prepare()
    await f.update.activate()
    expect(f.api.getUpdateStatus).not.toHaveBeenCalled()
    expect(f.api.performUpdate).not.toHaveBeenCalled()
    expect(f.api.restartService).not.toHaveBeenCalled()
  })

  it('stops polling after disposal and when an inactive panel is closed', async () => {
    const f = fixture(status('ready'))
    await settle()
    f.visible.value = false
    await nextTick()
    const calls = f.api.getUpdateStatus.mock.calls.length
    await vi.advanceTimersByTimeAsync(5000)
    expect(f.api.getUpdateStatus).toHaveBeenCalledTimes(calls)
    f.scope.stop()
    await vi.advanceTimersByTimeAsync(5000)
    expect(f.api.getUpdateStatus).toHaveBeenCalledTimes(calls)
  })
})
