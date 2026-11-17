import { describe, expect, it } from 'vitest'

import { getExpirationDateRelation, getRemainingExpiryDuration, getEffectiveWeeklyLimit, getQuotaUsagePercentage } from '../subscriptionQuota'
import type { Group } from '@/types'

describe('effective subscription quota', () => {
  const group = (limit: number | null) => ({ weekly_limit_usd: limit } as Group)
  it('freezes 50 immediately, preserving that freeze when the dynamic base falls from 400 to 370', () => {
    expect(getEffectiveWeeklyLimit({ group: group(400), quota_frozen_usd: 50 })).toBe(350)
    expect(getEffectiveWeeklyLimit({ group: group(370), quota_frozen_usd: 50 })).toBe(320)
    expect(getEffectiveWeeklyLimit({ group: group(370), quota_adjustment_usd: -50, quota_frozen_usd: 0 })).toBe(320)
    expect(getEffectiveWeeklyLimit({ group: group(370), quota_adjustment_usd: 50 })).toBe(420)
  })
  it('prefers the server effective limit including zero and negative values', () => {
    for (const limit of [0, -30, 350]) expect(getEffectiveWeeklyLimit({ group: group(400), effective_weekly_limit_usd: limit })).toBe(limit)
    expect(getEffectiveWeeklyLimit({ group: group(null) })).toBeNull()
    expect(getEffectiveWeeklyLimit({ group: group(0) })).toBeNull()
  })
  it.each([[0, -30, 100], [0, 0, 100], [50, 100, 50], [500, 100, 100], [-2, 100, 0], [0, null, 0], [NaN, 100, 0]])('renders safe progress for used %s / limit %s', (used, limit, result) => {
    expect(getQuotaUsagePercentage(used, limit)).toBe(result)
  })
})

describe('subscription expiry timing', () => {
  it('uses local calendar dates for today and tomorrow', () => {
    const now = new Date(2026, 2, 7, 23, 30)

    expect(getExpirationDateRelation(new Date(2026, 2, 7, 23, 45), now)).toBe('today')
    expect(getExpirationDateRelation(new Date(2026, 2, 8, 3, 30), now)).toBe('tomorrow')
  })

  it('treats the exact expiry instant and elapsed expiries as expired', () => {
    const now = new Date(2026, 6, 30, 9, 0)

    expect(getExpirationDateRelation(now, now)).toBe('expired')
    expect(getRemainingExpiryDuration(now, now)).toBeNull()
    expect(getExpirationDateRelation(new Date(2026, 6, 30, 8, 59), now)).toBe('expired')
    expect(getRemainingExpiryDuration(new Date(2026, 6, 30, 8, 59), now)).toBeNull()
  })

  it('rejects invalid target and current dates', () => {
    const invalid = new Date('invalid')
    const valid = new Date(2026, 6, 30, 9, 0)

    expect(getExpirationDateRelation(invalid, valid)).toBeNull()
    expect(getExpirationDateRelation(valid, invalid)).toBeNull()
    expect(getRemainingExpiryDuration(invalid, valid)).toBeNull()
    expect(getRemainingExpiryDuration(valid, invalid)).toBeNull()
  })

  it('returns rounded-up hours and minutes for an expiry under 24 hours away', () => {
    const now = new Date(2026, 6, 30, 9, 0)

    expect(getRemainingExpiryDuration(new Date(2026, 6, 31, 8, 30), now)).toEqual({
      unit: 'hoursMinutes',
      hours: 23,
      minutes: 30
    })
    expect(getRemainingExpiryDuration(new Date(now.getTime() + 1), now)).toEqual({
      unit: 'hoursMinutes',
      hours: 0,
      minutes: 1
    })
    expect(getRemainingExpiryDuration(new Date(now.getTime() + 23 * 60 * 60 * 1000 + 1), now)).toEqual({
      unit: 'hoursMinutes',
      hours: 23,
      minutes: 1
    })
  })

  it('preserves rounded-up day display from 24 hours onward', () => {
    const now = new Date(2026, 6, 30, 9, 0)

    expect(getRemainingExpiryDuration(new Date(now.getTime() + 24 * 60 * 60 * 1000), now)).toEqual({
      unit: 'days',
      days: 1
    })
    expect(getRemainingExpiryDuration(new Date(now.getTime() + 24 * 60 * 60 * 1000 + 1), now)).toEqual({
      unit: 'days',
      days: 2
    })
  })
})
