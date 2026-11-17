import { mount } from '@vue/test-utils'
import { describe, expect, it, vi } from 'vitest'
import CarpoolSettings from '../CarpoolSettings.vue'
import { applyCarpoolBonus, applyCarpoolMembers, isValidCarpoolBonus } from '../carpoolSettings'

vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))

const membersInput = '[aria-label="admin.groups.carpool.members"]'
const bonusInput = '[aria-label="admin.groups.carpool.bonus"]'

function form(limit: number | null = null, bonus?: number) {
  return { carpool_members: 0, carpool_bonus_usd: bonus, name: 'old', description: '', daily_limit_usd: 50 as number | null, weekly_limit_usd: limit, monthly_limit_usd: 1000 as number | null }
}

describe('carpool settings', () => {
  it.each([null, 0, -10])('initializes missing/nonpositive caps (%s)', (limit) => {
    const state = form(limit)
    applyCarpoolMembers(state, 4)
    expect(state).toMatchObject({ carpool_members: 4, name: '4 人车分组', description: '4 人均分额度', daily_limit_usd: null, monthly_limit_usd: null, weekly_limit_usd: 500 })
  })
  it('keeps the existing cap on enable, reopen and size change', () => {
    const state = form(325)
    applyCarpoolMembers(state, 4)
    applyCarpoolMembers(state, 4)
    applyCarpoolMembers(state, 3)
    expect(state.weekly_limit_usd).toBe(325)
    expect(state.name).toBe('3 人车分组')
  })
  it.each([[2, 900], [3, 633.33], [4, 500], [5, 420]])('uses the backend 1600 estimate default for %i members', (members, expected) => {
    const state = form()
    applyCarpoolMembers(state, members)
    expect(state.weekly_limit_usd).toBe(expected)
  })
  it.each([-1, 1, 6, 2.5, NaN, Infinity])('ignores an invalid member count %s', (members) => {
    const state = form()
    const original = { ...state }
    applyCarpoolMembers(state, members)
    expect(state).toEqual(original)
  })
  it('disables without destroying the cap', () => {
    const state = form(325)
    applyCarpoolMembers(state, 4)
    applyCarpoolMembers(state, 0)
    expect(state.carpool_members).toBe(0)
    expect(state.weekly_limit_usd).toBe(325)
  })
  it.each([[0, 533.33], [150.29, 683.62], [1e10, 10000000533.33]])('bootstraps with configured bonus %s', (bonus, expected) => {
    const state = form(null, bonus)
    applyCarpoolMembers(state, 3)
    expect(state.weekly_limit_usd).toBe(expected)
    expect(state.carpool_bonus_usd).toBe(bonus)
  })
  it.each([0, 0.01, 0.29, 100, 150.25, 9999999999.99, 1e10])('accepts a finite cent-precision bonus %s', (bonus) => {
    expect(isValidCarpoolBonus(bonus)).toBe(true)
  })
  it.each(['', ' ', '100', 'NaN', 'Infinity', null, undefined, true, -0.01, -1, NaN, Infinity, -Infinity, 1e10 + 0.01, 1.001, 0.001, 100.0000000001])('rejects an invalid bonus %s', (bonus) => {
    expect(isValidCarpoolBonus(bonus)).toBe(false)
    const state = form(325.29)
    state.carpool_members = 4
    const original = { ...state }
    applyCarpoolBonus(state, bonus as number)
    expect(state).toEqual(original)
  })
  it('previews against the current cap without accumulating changes twice or drifting', () => {
    const state = form(325.29)
    state.carpool_members = 4
    applyCarpoolBonus(state, 150.25)
    expect(state.weekly_limit_usd).toBe(375.54)
    applyCarpoolBonus(state, 150.25)
    expect(state.weekly_limit_usd).toBe(375.54)
    applyCarpoolMembers(state, 3)
    expect(state.weekly_limit_usd).toBe(375.54)
    applyCarpoolBonus(state, 0)
    expect(state.weekly_limit_usd).toBe(225.29)
    applyCarpoolBonus(state, 100)
    expect(state.weekly_limit_usd).toBe(325.29)
    expect(state.carpool_bonus_usd).toBe(100)
  })
  it('preserves zero bonus as the delta baseline', () => {
    const state = form(325.29, 0)
    state.carpool_members = 4
    applyCarpoolBonus(state, 0.29)
    expect(state.weekly_limit_usd).toBe(325.58)
    applyCarpoolBonus(state, 0)
    expect(state.weekly_limit_usd).toBe(325.29)
  })
  it('does not bootstrap again when the preview reaches zero', () => {
    const state = form(100, 100)
    state.carpool_members = 4
    applyCarpoolBonus(state, 0)
    expect(state.weekly_limit_usd).toBe(0)
    applyCarpoolBonus(state, 100)
    expect(state.weekly_limit_usd).toBe(100)
  })
  it('does not invent a missing cap on bonus edits', () => {
    const state = form()
    state.carpool_members = 4
    applyCarpoolBonus(state, 150)
    expect(state.weekly_limit_usd).toBeNull()
    expect(state.carpool_bonus_usd).toBe(150)
  })
  it('changes only the configured bonus while disabled', () => {
    const state = form(325.29)
    applyCarpoolBonus(state, 150)
    expect(state).toEqual({ ...form(325.29), carpool_bonus_usd: 150 })
  })
  it('does not emit changes on mount', async () => {
    const wrapper = mount(CarpoolSettings, { props: { members: 4 } })
    expect(wrapper.emitted('update:members')).toBeUndefined()
    await wrapper.get(membersInput).setValue('3')
    expect(wrapper.emitted('update:members')).toEqual([[3]])
    wrapper.unmount()
  })
  it.each([undefined, 0, 150.25])('hydrates bonus %s without emitting or recalculating', async (bonus) => {
    const wrapper = mount(CarpoolSettings, { props: { members: 4, bonus } })
    expect(wrapper.get<HTMLInputElement>(bonusInput).element.value).toBe(String(bonus ?? 100))
    expect(wrapper.emitted('update:bonus')).toBeUndefined()
    expect(wrapper.emitted('update:members')).toBeUndefined()
    await wrapper.setProps({ bonus: 175.29 })
    expect(wrapper.get<HTMLInputElement>(bonusInput).element.value).toBe('175.29')
    expect(wrapper.emitted('update:bonus')).toBeUndefined()
    wrapper.unmount()
  })
  it.each(['0', '0.29', '150.25', '10000000000'])('emits legal bonus %s immediately on input', async (value) => {
    const wrapper = mount(CarpoolSettings, { props: { members: 4, bonus: 100 } })
    const input = wrapper.get<HTMLInputElement>(bonusInput)
    expect(input.attributes()).toMatchObject({ type: 'number', min: '0', max: '10000000000', step: '0.01', required: '' })
    input.element.value = value
    await input.trigger('input')
    expect(wrapper.emitted('update:bonus')).toEqual([[Number(value)]])
    expect(input.element.checkValidity()).toBe(true)
    expect(wrapper.text()).toContain('admin.groups.carpool.bonusHint')
    expect(wrapper.text()).toContain('admin.groups.carpool.scheduleValue')
    wrapper.unmount()
  })
  it.each(['', '-1', '-0.01', '0.001', '100.001', 'NaN', 'Infinity', '1e309', '10000000000.01'])('rejects bonus input %s and recovers after correction', async (value) => {
    const wrapper = mount(CarpoolSettings, { props: { members: 4, bonus: 100 } })
    const input = wrapper.get<HTMLInputElement>(bonusInput)
    await input.setValue(value)
    expect(wrapper.emitted('update:bonus')).toBeUndefined()
    expect(wrapper.vm.validate()).toBe(false)
    expect(input.element.checkValidity()).toBe(false)
    expect(input.attributes('aria-invalid')).toBe('true')
    expect(wrapper.get('[role=alert]').text()).toBe('admin.groups.carpool.bonusInvalid')
    await input.setValue('0')
    expect(wrapper.vm.validate()).toBe(true)
    expect(input.element.checkValidity()).toBe(true)
    expect(wrapper.emitted('update:bonus')).toEqual([[0]])
    wrapper.unmount()
  })
  it('only shows the bonus field when enabled and clears invalid local drafts on re-enable', async () => {
    const wrapper = mount(CarpoolSettings, { props: { members: 0, bonus: 0 } })
    expect(wrapper.find(bonusInput).exists()).toBe(false)
    await wrapper.setProps({ members: 4 })
    expect(wrapper.get<HTMLInputElement>(bonusInput).element.value).toBe('0')
    await wrapper.get(bonusInput).setValue('')
    expect(wrapper.vm.validate()).toBe(false)
    await wrapper.setProps({ members: 0 })
    expect(wrapper.find(bonusInput).exists()).toBe(false)
    expect(wrapper.vm.validate()).toBe(true)
    await wrapper.setProps({ members: 4 })
    expect(wrapper.get<HTMLInputElement>(bonusInput).element.value).toBe('0')
    expect(wrapper.vm.validate()).toBe(true)
    expect(wrapper.emitted('update:bonus')).toBeUndefined()
    wrapper.unmount()
  })
  it('does not clear an invalid bonus draft when changing the member count', async () => {
    const wrapper = mount(CarpoolSettings, { props: { members: 4, bonus: 100 } })
    await wrapper.get(bonusInput).setValue('')
    await wrapper.setProps({ members: 3 })
    expect(wrapper.get<HTMLInputElement>(bonusInput).element.value).toBe('')
    expect(wrapper.vm.validate()).toBe(false)
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
    await wrapper.get(membersInput).setValue(value)
    expect(wrapper.emitted('update:members')).toBeUndefined()
    wrapper.unmount()
  })
})
