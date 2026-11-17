import { beforeEach, describe, expect, it, vi } from 'vitest'
import * as user from '../quotaTrades'
import * as admin from '../admin/quotaTrades'

const { get, post } = vi.hoisted(() => ({ get: vi.fn(), post: vi.fn() }))
vi.mock('../client', () => ({ apiClient: { get, post } }))
beforeEach(() => vi.clearAllMocks())

describe('quota trading API', () => {
  it('lists subscription trades with pagination and cancellation', async () => {
    const signal = new AbortController().signal
    const data = { enabled: true, items: [], total: 0, page: 2, page_size: 20 }
    get.mockResolvedValue({ data })
    expect(await user.listQuotaTrades(12, { page: 2, page_size: 20 }, { signal })).toEqual(data)
    expect(get).toHaveBeenCalledWith('/subscriptions/12/quota-trades', { params: { page: 2, page_size: 20 }, signal })
  })
  it.each(['current', 'history'] as const)('passes the %s view for server-side pagination', async view => {
    get.mockResolvedValue({ data: { items: [], total: 0 } })
    await user.listQuotaTrades(12, { page: 1, page_size: 20, view })
    expect(get).toHaveBeenCalledWith('/subscriptions/12/quota-trades', { params: { page: 1, page_size: 20, view }, signal: undefined })
  })
  it('passes accepted terms, amount, total sale price, and a stable caller-provided UUID', async () => {
    const request = { amount_usd: 50, sale_price_cny: 15, request_id: '3d6b25e4-5b6e-4567-9cf8-e12345678901', accepted_terms: true as const }
    post.mockResolvedValue({ data: { id: 42 } })
    expect(await user.createQuotaTrade(12, request)).toEqual({ id: 42 })
    expect(post).toHaveBeenCalledWith('/subscriptions/12/quota-trades', request)
  })
  it.each([
    ['reserve', user.reserveQuotaTrade], ['confirm', user.confirmQuotaTrade], ['withdraw', user.withdrawQuotaTrade]
  ] as const)('posts user %s to the correct scoped listing', async (action, fn) => {
    post.mockResolvedValue({ data: { id: 42 } })
    expect(await fn(12, 42)).toEqual({ id: 42 })
    expect(post).toHaveBeenCalledWith(`/subscriptions/12/quota-trades/42/${action}`, {})
  })
  it('passes every admin filter and pagination', async () => {
    const params = { page: 3, page_size: 50, status: 'reserved' as const, group_id: 1, account_id: 2, user_id: 3 }
    const signal = new AbortController().signal
    get.mockResolvedValue({ data: { items: [], total: 0 } })
    await admin.listQuotaTrades(params, { signal })
    expect(get).toHaveBeenCalledWith('/admin/carpool-quota-trades', { params, signal })
  })
  it('returns event arrays already decoded by the shared client', async () => {
    const signal = new AbortController().signal
    const events = [{ id: 1, action: 'confirm', is_admin: true }]
    get.mockResolvedValue({ data: events })
    expect(await admin.getQuotaTradeEvents(42, { signal })).toEqual(events)
    expect(get).toHaveBeenCalledWith('/admin/carpool-quota-trades/42/events', { signal })
  })
  it.each([
    ['confirm', admin.confirmQuotaTrade], ['cancel-reservation', admin.cancelQuotaTradeReservation], ['withdraw', admin.withdrawQuotaTrade]
  ] as const)('posts admin %s without inventing payment fields', async (action, fn) => {
    post.mockResolvedValue({ data: { id: 42 } })
    expect(await fn(42)).toEqual({ id: 42 })
    expect(post).toHaveBeenCalledWith(`/admin/carpool-quota-trades/42/${action}`, {})
  })
  it('preserves normalized backend errors for existing UI error handling', async () => {
    const error = { status: 409, message: 'Listing is reserved' }
    post.mockRejectedValue(error)
    await expect(user.withdrawQuotaTrade(12, 42)).rejects.toBe(error)
    await expect(admin.withdrawQuotaTrade(42)).rejects.toBe(error)
  })
})
