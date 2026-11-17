import { apiClient } from './client'
import type { CreateQuotaTradeRequest, QuotaTrade, QuotaTradeView, SubscriptionQuotaTrades } from '@/types/quotaTrade'

export async function listQuotaTrades(
  subscriptionId: number,
  params: { page: number; page_size: number; view?: QuotaTradeView },
  options?: { signal?: AbortSignal }
): Promise<SubscriptionQuotaTrades> {
  const { data } = await apiClient.get<SubscriptionQuotaTrades>(
    `/subscriptions/${subscriptionId}/quota-trades`, { params, signal: options?.signal }
  )
  return data
}

export async function createQuotaTrade(subscriptionId: number, request: CreateQuotaTradeRequest): Promise<QuotaTrade> {
  const { data } = await apiClient.post<QuotaTrade>(`/subscriptions/${subscriptionId}/quota-trades`, request)
  return data
}

async function tradeAction(subscriptionId: number, tradeId: number, action: 'reserve' | 'confirm' | 'withdraw'): Promise<QuotaTrade> {
  const { data } = await apiClient.post<QuotaTrade>(`/subscriptions/${subscriptionId}/quota-trades/${tradeId}/${action}`, {})
  return data
}

export const reserveQuotaTrade = (subscriptionId: number, tradeId: number) => tradeAction(subscriptionId, tradeId, 'reserve')
export const confirmQuotaTrade = (subscriptionId: number, tradeId: number) => tradeAction(subscriptionId, tradeId, 'confirm')
export const withdrawQuotaTrade = (subscriptionId: number, tradeId: number) => tradeAction(subscriptionId, tradeId, 'withdraw')
