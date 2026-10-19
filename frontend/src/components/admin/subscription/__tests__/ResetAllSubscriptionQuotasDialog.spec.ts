import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import type { SubscriptionBulkActionRequest, SubscriptionBulkActionResult } from '@/api/admin/subscriptions'
import ResetAllSubscriptionQuotasDialog from '../ResetAllSubscriptionQuotasDialog.vue'

const { getResetQuotaTargets, bulkAction } = vi.hoisted(() => ({ getResetQuotaTargets: vi.fn(), bulkAction: vi.fn() }))
vi.mock('@/api/admin', () => ({ adminAPI: { subscriptions: { getResetQuotaTargets, bulkAction } } }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({
  t: (key: string, params?: Record<string, unknown>) => params ? `${key} ${JSON.stringify(params)}` : key
}) }))

let adminId = 5000
const success = (request: SubscriptionBulkActionRequest): SubscriptionBulkActionResult => ({
  success_count: request.subscription_ids.length, failed_count: 0,
  results: request.subscription_ids.map(subscription_id => ({ subscription_id, success: true }))
})
function mountDialog() {
  const wrapper = mount(ResetAllSubscriptionQuotasDialog, {
    global: { stubs: {
      Icon: true,
      BaseDialog: {
        name: 'BaseDialog', props: ['show', 'closeOnEscape', 'showCloseButton'], emits: ['close'],
        template: '<section><slot /><footer><slot name="footer" /></footer></section>'
      }
    } }
  })
  wrappers.push(wrapper)
  return wrapper
}
const wrappers: ReturnType<typeof mountDialog>[] = []

beforeEach(() => {
  getResetQuotaTargets.mockReset().mockResolvedValue({ subscription_ids: [1, 2] })
  bulkAction.mockReset().mockImplementation(async request => success(request))
  localStorage.setItem('auth_user', JSON.stringify({ id: ++adminId }))
  sessionStorage.clear()
})
afterEach(() => wrappers.splice(0).forEach(wrapper => wrapper.unmount()))

describe('ResetAllSubscriptionQuotasDialog', () => {
  it('shows the global scope and target count before any writes', async () => {
    const wrapper = mountDialog()
    await flushPromises()
    expect(wrapper.text()).toContain('admin.subscriptions.resetAll.scope')
    expect(wrapper.text()).toContain('"count":2')
    expect(wrapper.text()).toContain('admin.subscriptions.resetAll.warning')
    expect(bulkAction).not.toHaveBeenCalled()
    await wrapper.get('[data-test="reset-all-confirm"]').trigger('click')
    await flushPromises()
    expect(bulkAction).toHaveBeenCalledTimes(1)
    expect(wrapper.emitted('updated')).toHaveLength(1)
    expect(wrapper.text()).toContain('admin.subscriptions.resetAll.completed')
  })

  it('blocks repeated clicks and dismissal during a request', async () => {
    let resolve!: (result: SubscriptionBulkActionResult) => void
    bulkAction.mockImplementationOnce(() => new Promise(done => { resolve = done }))
    const wrapper = mountDialog()
    await flushPromises()
    await wrapper.get('[data-test="reset-all-confirm"]').trigger('click')
    await wrapper.get('[data-test="reset-all-confirm"]').trigger('click')
    const dialog = wrapper.getComponent({ name: 'BaseDialog' })
    dialog.vm.$emit('close')
    expect(bulkAction).toHaveBeenCalledTimes(1)
    expect(dialog.props('closeOnEscape')).toBe(false)
    expect(dialog.props('showCloseButton')).toBe(false)
    expect(wrapper.get('[data-test="reset-all-close"]').attributes('disabled')).toBeDefined()
    expect(wrapper.emitted('close')).toBeUndefined()
    resolve(success(bulkAction.mock.calls[0]![0]))
    await flushPromises()
    await wrapper.get('[data-test="reset-all-close"]').trigger('click')
    expect(wrapper.emitted('close')).toHaveLength(1)
  })

  it('shows per-item errors and exposes retry for failed items only', async () => {
    bulkAction.mockResolvedValueOnce({ success_count: 1, failed_count: 1, results: [
      { subscription_id: 1, success: true }, { subscription_id: 2, success: false, error: 'Reset failed' }
    ] })
    const wrapper = mountDialog()
    await flushPromises()
    await wrapper.get('[data-test="reset-all-confirm"]').trigger('click')
    await flushPromises()
    expect(wrapper.text()).toContain('Reset failed')
    expect(wrapper.text()).toContain('#2')
    await wrapper.get('[data-test="reset-all-retry"]').trigger('click')
    await flushPromises()
    expect(bulkAction.mock.calls[1]![0].subscription_ids).toEqual([2])
    expect(wrapper.find('[data-test="reset-all-retry"]').exists()).toBe(false)
    expect(wrapper.emitted('updated')).toHaveLength(2)
  })

  it('keeps the same operation available after an uncertain response', async () => {
    bulkAction.mockRejectedValueOnce({ status: 503, message: 'Connection lost' })
    const wrapper = mountDialog()
    await flushPromises()
    await wrapper.get('[data-test="reset-all-confirm"]').trigger('click')
    await flushPromises()
    expect(wrapper.text()).toContain('Connection lost')
    await wrapper.get('[data-test="reset-all-confirm"]').trigger('click')
    await flushPromises()
    expect(bulkAction.mock.calls[1]).toEqual(bulkAction.mock.calls[0])
  })

  it('offers collection retry after a list error and does not reset partial targets', async () => {
    getResetQuotaTargets.mockRejectedValueOnce(new Error('List unavailable'))
    const wrapper = mountDialog()
    await flushPromises()
    expect(wrapper.text()).toContain('List unavailable')
    expect(wrapper.find('[data-test="reset-all-confirm"]').exists()).toBe(false)
    await wrapper.get('[data-test="reset-all-reload"]').trigger('click')
    await flushPromises()
    expect(wrapper.get('[data-test="reset-all-confirm"]').attributes('disabled')).toBeUndefined()
    expect(bulkAction).not.toHaveBeenCalled()
  })

  it('shows an empty state instead of a reset button when nothing is eligible', async () => {
    getResetQuotaTargets.mockResolvedValueOnce({ subscription_ids: [] })
    const wrapper = mountDialog()
    await flushPromises()
    expect(wrapper.text()).toContain('admin.subscriptions.resetAll.empty')
    expect(wrapper.find('[data-test="reset-all-confirm"]').exists()).toBe(false)
    expect(bulkAction).not.toHaveBeenCalled()
  })
})
