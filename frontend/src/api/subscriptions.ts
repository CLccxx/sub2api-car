/**
 * User Subscription API
 * API for regular users to view their own subscriptions and progress
 */

import { apiClient } from './client'
import type { UserSubscription, SubscriptionProgress } from '@/types'

export interface SharedAccountUsage {
  status: 'ready' | 'no_data' | 'unavailable' | 'not_configured' | 'multiple_accounts' | 'unsupported_account'
  updated_at: string | null
  refresh_available_at: string | null
  seven_day: {
    utilization: number
    resets_at: string | null
    estimated_total_cost: number | null
  } | null
  reset_credits: number | null
  credits: { has_credits: boolean; unlimited: boolean; balance: string | null } | null
}

export async function getSharedAccountUsage(
  subscriptionId: number,
  options?: { signal?: AbortSignal }
): Promise<SharedAccountUsage> {
  const { data } = await apiClient.get<SharedAccountUsage>(
    `/subscriptions/${subscriptionId}/shared-usage`, { signal: options?.signal }
  )
  return data
}

export interface ResetVoteView {
  required_count: number
  member_count: number
  can_create: boolean
  blocked_reason: string | null
  request_resolved?: boolean
  proposal: {
    id: number
    status: 'voting' | 'executing' | 'awaiting_confirmation' | 'applying' | 'completed' | 'expired' | 'invalidated' | 'failed'
    agreed_count: number
    required_count: number
    my_vote: boolean
    can_agree: boolean
    expires_at: string
    reason: string | null
  } | null
}

export async function getResetVote(
  subscriptionId: number,
  options?: { signal?: AbortSignal; requestId?: string }
): Promise<ResetVoteView> {
  const { data } = await apiClient.get<ResetVoteView>(
    `/subscriptions/${subscriptionId}/reset-vote`, {
      signal: options?.signal,
      ...(options?.requestId ? { params: { request_id: options.requestId } } : {})
    }
  )
  return data
}

export async function createResetVote(
  subscriptionId: number,
  requestId: string,
  options?: { signal?: AbortSignal }
): Promise<ResetVoteView> {
  const { data } = await apiClient.post<ResetVoteView>(
    `/subscriptions/${subscriptionId}/reset-vote`, { request_id: requestId }, { signal: options?.signal }
  )
  return data
}

export async function agreeResetVote(
  subscriptionId: number,
  proposalId: number,
  options?: { signal?: AbortSignal }
): Promise<ResetVoteView> {
  const { data } = await apiClient.post<ResetVoteView>(
    `/subscriptions/${subscriptionId}/reset-vote/${proposalId}/agree`, {}, { signal: options?.signal }
  )
  return data
}

/**
 * Subscription summary for user dashboard
 */
export interface SubscriptionSummary {
  active_count: number
  subscriptions: Array<{
    id: number
    group_name: string
    status: string
    daily_progress: number | null
    weekly_progress: number | null
    monthly_progress: number | null
    expires_at: string | null
    days_remaining: number | null
  }>
}

/**
 * Get list of current user's subscriptions
 */
export async function getMySubscriptions(options?: { signal?: AbortSignal }): Promise<UserSubscription[]> {
  const response = await apiClient.get<UserSubscription[]>('/subscriptions', options)
  return response.data
}

/**
 * Get current user's active subscriptions
 */
export async function getActiveSubscriptions(): Promise<UserSubscription[]> {
  const response = await apiClient.get<UserSubscription[]>('/subscriptions/active')
  return response.data
}

/**
 * Get progress for all user's active subscriptions
 */
export async function getSubscriptionsProgress(): Promise<SubscriptionProgress[]> {
  const response = await apiClient.get<SubscriptionProgress[]>('/subscriptions/progress')
  return response.data
}

/**
 * Get subscription summary for dashboard display
 */
export async function getSubscriptionSummary(): Promise<SubscriptionSummary> {
  const response = await apiClient.get<SubscriptionSummary>('/subscriptions/summary')
  return response.data
}

/**
 * Get progress for a specific subscription
 */
export async function getSubscriptionProgress(
  subscriptionId: number
): Promise<SubscriptionProgress> {
  const response = await apiClient.get<SubscriptionProgress>(
    `/subscriptions/${subscriptionId}/progress`
  )
  return response.data
}

export default {
  getResetVote,
  createResetVote,
  agreeResetVote,
  getSharedAccountUsage,
  getMySubscriptions,
  getActiveSubscriptions,
  getSubscriptionsProgress,
  getSubscriptionSummary,
  getSubscriptionProgress
}
