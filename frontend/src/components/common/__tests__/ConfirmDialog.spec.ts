import { afterEach, describe, expect, it, vi } from 'vitest'
import { enableAutoUnmount, mount } from '@vue/test-utils'
import ConfirmDialog from '../ConfirmDialog.vue'
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))
enableAutoUnmount(afterEach)
const open = (props = {}) => mount(ConfirmDialog, {
  props: { show: true, title: 'Confirm', message: 'Verify', ...props },
  global: { stubs: { BaseDialog: { name: 'BaseDialog', props: ['show'], template: '<div><slot /><slot name="footer" /></div>' } } }
})
describe('shared confirmation dialog optional safeguards', () => {
  it('retains the existing enabled confirm/cancel behavior by default', async () => {
    const w = open()
    await w.get('button:last-child').trigger('click')
    await w.get('button:first-child').trigger('click')
    expect(w.emitted('confirm')).toHaveLength(1)
    expect(w.emitted('cancel')).toHaveLength(1)
  })
  it('disables confirmation only, allowing cancellation when acknowledgement is required', async () => {
    const w = open({ confirmDisabled: true })
    expect(w.get('button:last-child').attributes('disabled')).toBeDefined()
    await w.get('button:last-child').trigger('click')
    await w.get('button:first-child').trigger('click')
    expect(w.emitted('confirm')).toBeUndefined()
    expect(w.emitted('cancel')).toHaveLength(1)
  })
  it('blocks duplicate confirmation and cancellation, including dialog-close events, while busy', async () => {
    const w = open({ busy: true })
    for (const button of w.findAll('button')) {
      expect(button.attributes('disabled')).toBeDefined()
      await button.trigger('click')
    }
    w.findComponent({ name: 'BaseDialog' }).vm.$emit('close')
    expect(w.emitted('confirm')).toBeUndefined()
    expect(w.emitted('cancel')).toBeUndefined()
  })
})
