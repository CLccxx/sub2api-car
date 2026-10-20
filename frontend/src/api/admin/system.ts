/**
 * System API endpoints for admin operations
 */

import { apiClient } from '../client'

export interface ReleaseInfo {
  name: string
  body: string
  published_at: string
  html_url: string
}

export interface VersionInfo {
  current_version: string
  latest_version: string
  has_update: boolean
  release_info?: ReleaseInfo
  cached: boolean
  warning?: string
  build_type: string // "source" for manual builds, "release" for CI builds
  repository?: string
  update_mode?: 'managed' | 'binary'
}

/**
 * Get current version
 */
export async function getVersion(): Promise<{ version: string }> {
  const { data } = await apiClient.get<{ version: string }>('/admin/system/version')
  return data
}

/**
 * Check for updates
 * @param force - Force refresh from GitHub API
 */
export async function checkUpdates(force = false): Promise<VersionInfo> {
  const { data } = await apiClient.get<VersionInfo>('/admin/system/check-updates', {
    params: force ? { force: 'true' } : undefined
  })
  return data
}

export interface UpdateResult {
  message: string
  need_restart: boolean
  operation_id?: string
  update_mode?: 'managed' | 'binary'
  status?: ManagedUpdateStatus
}

export type ManagedUpdatePhase = 'idle' | 'preparing' | 'ready' | 'applying' | 'succeeded' | 'failed' | 'rolled_back' | 'manual_recovery_required'
export interface ManagedUpdateStatus {
  protocol_version: 1
  phase: ManagedUpdatePhase
  operation_id: string
  target_version: string
  target_commit: string
  current_version: string
  message: string
  error_code: string
  backup_path: string
  step?: string
}

export async function getUpdateStatus(): Promise<ManagedUpdateStatus> {
  const { data } = await apiClient.get<ManagedUpdateStatus>('/admin/system/update-status', { timeout: 15000 })
  return data
}

export interface RollbackVersionInfo {
  version: string
  published_at: string
  html_url: string
}

/**
 * Get versions available for rollback (up to 3 versions older than current)
 */
export async function getRollbackVersions(): Promise<{ versions: RollbackVersionInfo[] }> {
  const { data } = await apiClient.get<{ versions: RollbackVersionInfo[] }>(
    '/admin/system/rollback-versions'
  )
  return data
}

/**
 * In-place update/rollback downloads a full release binary from GitHub, which
 * can take several minutes on slow links. The global 30s axios timeout would
 * abort the request mid-download (#4504), so these calls wait as long as the
 * backend allows (15 minutes server-side).
 */
const UPDATE_REQUEST_TIMEOUT_MS = 15 * 60 * 1000

/**
 * Perform system update
 * Downloads and applies the latest version
 */
export async function performUpdate(): Promise<UpdateResult> {
  const { data } = await apiClient.post<UpdateResult>('/admin/system/update', undefined, {
    timeout: UPDATE_REQUEST_TIMEOUT_MS
  })
  return data
}

/**
 * Rollback to a previous version
 * @param version - Target version (e.g. "0.1.146"); omit to restore the local backup binary
 */
export async function rollback(version?: string): Promise<UpdateResult> {
  const { data } = await apiClient.post<UpdateResult>(
    '/admin/system/rollback',
    version ? { version } : undefined,
    { timeout: UPDATE_REQUEST_TIMEOUT_MS }
  )
  return data
}

/**
 * Restart the service
 */
export async function restartService(operationID?: string): Promise<{ message: string; status?: ManagedUpdateStatus }> {
  const { data } = await apiClient.post<{ message: string; status?: ManagedUpdateStatus }>('/admin/system/restart', operationID ? { operation_id: operationID } : undefined)
  return data
}

export const systemAPI = {
  getVersion,
  checkUpdates,
  getUpdateStatus,
  performUpdate,
  getRollbackVersions,
  rollback,
  restartService
}

export default systemAPI
