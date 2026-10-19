import { mount } from '@vue/test-utils'
import { describe, expect, it, vi } from 'vitest'
import CarpoolSettings from '../CarpoolSettings.vue'
import { applyCarpoolMembers } from '../carpoolSettings'

vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))

function form(limit: number | null = null) {
  return { carpool_members: 0, name: 'old', description: '', daily_limit_usd: 50 as number | null, weekly_limit_usd: limit, monthly_limit_usd: 1000 as number | null }
}

describe('carpool settings', () => {
  it.each([null, 0])('fills only missing/zero caps (%s)', (limit) => {
    const state = form(limit)
    applyCarpoolMembers(state, 4)
    expect(state).toMatchObject({ carpool_members: 4, name: '4 人车分组', description: '4 人均分额度', daily_limit_usd: null, monthly_limit_usd: null, weekly_limit_usd: 400 })
  })
  it('keeps the existing cap on enable, reopen and size change', () => {
    const state = form(325)
    applyCarpoolMembers(state, 4)
    applyCarpoolMembers(state, 4)
    applyCarpoolMembers(state, 3)
    expect(state.weekly_limit_usd).toBe(325)
    expect(state.name).toBe('3 人车分组')
  })
  it('rounds fractional defaults down to cents', () => {
    const state = form()
    applyCarpoolMembers(state, 3)
    expect(state.weekly_limit_usd).toBe(533.33)
  })
  it('disables without destroying the cap', () => {
    const state = form(325)
    applyCarpoolMembers(state, 4)
    applyCarpoolMembers(state, 0)
    expect(state.carpool_members).toBe(0)
    expect(state.weekly_limit_usd).toBe(325)
  })
  it('does not emit changes on mount', async () => {
    const wrapper = mount(CarpoolSettings, { props: { members: 4 } })
    expect(wrapper.emitted('update:members')).toBeUndefined()
    await wrapper.get('input[type=number]').setValue('3')
    expect(wrapper.emitted('update:members')).toEqual([[3]])
    wrapper.unmount()
  })
  it('uses a named toggle and defaults to four members when enabled', async () => {
    const wrapper = mount(CarpoolSettings, { props: { members: 0 } })
    expect(wrapper.get('[role=switch]').attributes('aria-label')).toBe('admin.groups.carpool.enabled')
    await wrapper.get('[role=switch]').trigger('click')
    expect(wrapper.emitted('update:members')).toEqual([[4]])
    wrapper.unmount()
  })
  it.each(['1', '6', '2.5', ''])('does not submit an invalid size %s', async (value) => {
    const wrapper = mount(CarpoolSettings, { props: { members: 4 } })
    await wrapper.get('input[type=number]').setValue(value)
    expect(wrapper.emitted('update:members')).toBeUndefined()
    wrapper.unmount()
  })
})
