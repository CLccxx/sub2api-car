export type QuotaTradeView = 'current' | 'history'

export type QuotaTradeStatus = 'open' | 'reserved' | 'completed' | 'withdrawn' | 'expired'

export interface QuotaTrade {
  id: number
  group_id: number
  group_name: string
  account_id: number
  cycle_id: string
  seller_user_id: number
  seller_name: string
  seller_subscription_id: number
  buyer_user_id: number | null
  buyer_name: string | null
  amount_usd: number
  sale_price_cny?: number | null
  status: QuotaTradeStatus
  can_action?: boolean
  created_at: string
  reserved_at: string | null
  completed_at: string | null
  terms_version: string
  terms_accepted_at?: string
}

export interface QuotaTradeEvent {
  id: number
  action: string
  actor_user_id: number | null
  actor_name: string
  is_admin: boolean
  buyer_user_id?: number | null
  created_at: string
}

export interface QuotaTradeQuota {
  base_limit_usd: number
  adjustment_usd: number
  frozen_usd: number
  used_usd: number
  available_usd: number
}

export interface QuotaTradePage {
  items: QuotaTrade[]
  total: number
  page: number
  page_size: number
}

export interface SubscriptionQuotaTrades extends QuotaTradePage {
  enabled: boolean
  terms_version?: string
  terms_notice?: string
  quota: QuotaTradeQuota
}

export interface CreateQuotaTradeRequest {
  amount_usd: number
  sale_price_cny: number
  request_id: string
  accepted_terms: true
}

export interface AdminQuotaTradeFilters {
  page?: number
  page_size?: number
  status?: QuotaTradeStatus
  group_id?: number
  account_id?: number
  user_id?: number
}
