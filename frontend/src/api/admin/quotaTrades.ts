import { apiClient } from '../client'
import type { AdminQuotaTradeFilters, QuotaTrade, QuotaTradeEvent, QuotaTradePage } from '@/types/quotaTrade'

const basePath = '/admin/carpool-quota-trades'

export async function listQuotaTrades(params: AdminQuotaTradeFilters, options?: { signal?: AbortSignal }): Promise<QuotaTradePage> {
  const { data } = await apiClient.get<QuotaTradePage>(basePath, { params, signal: options?.signal })
  return data
}

export async function getQuotaTradeEvents(tradeId: number, options?: { signal?: AbortSignal }): Promise<QuotaTradeEvent[]> {
  // The shared client already unwraps the standard { code, message, data } envelope.
  const { data } = await apiClient.get<QuotaTradeEvent[]>(`${basePath}/${tradeId}/events`, { signal: options?.signal })
  return data
}

async function tradeAction(tradeId: number, action: 'confirm' | 'cancel-reservation' | 'withdraw'): Promise<QuotaTrade> {
  const { data } = await apiClient.post<QuotaTrade>(`${basePath}/${tradeId}/${action}`, {})
  return data
}

export const confirmQuotaTrade = (tradeId: number) => tradeAction(tradeId, 'confirm')
export const cancelQuotaTradeReservation = (tradeId: number) => tradeAction(tradeId, 'cancel-reservation')
export const withdrawQuotaTrade = (tradeId: number) => tradeAction(tradeId, 'withdraw')
