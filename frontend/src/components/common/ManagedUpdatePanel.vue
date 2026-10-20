<template>
  <div class="space-y-3" aria-live="polite">
    <div v-if="warning" class="rounded-lg bg-amber-50 p-3 text-xs leading-5 text-amber-700 dark:bg-amber-900/20 dark:text-amber-300">
      {{ t('version.checkFailed') }}
      <p class="mt-1 break-words">{{ warning }}</p>
    </div>
    <div v-if="status && status.phase !== 'idle'" class="rounded-lg bg-gray-50 p-3 dark:bg-dark-700">
      <p class="text-sm font-medium text-gray-800 dark:text-dark-100">{{ t(`version.managed.${status.phase}`) }}</p>
      <p class="mt-1 text-xs text-gray-500 dark:text-dark-300">v{{ status.target_version }}</p>
      <p class="mt-2 text-xs leading-5 text-gray-600 dark:text-dark-300">{{ t(`version.managed.${status.phase}Hint`) }}</p>
      <p v-if="status.error_code && status.message" class="mt-2 break-words text-xs text-red-600 dark:text-red-400">{{ status.message }}</p>
      <p v-if="status.phase === 'manual_recovery_required' && status.backup_path" class="mt-2 break-all text-xs text-gray-500">{{ status.backup_path }}</p>
    </div>
    <p v-if="reconnecting" class="text-xs leading-5 text-amber-600 dark:text-amber-400">{{ t(connectionDelayed ? 'version.managed.connectionDelayed' : 'version.managed.reconnecting') }}</p>
    <p v-else-if="error" role="alert" class="break-words rounded-lg bg-red-50 p-3 text-xs leading-5 text-red-600 dark:bg-red-900/20 dark:text-red-400">{{ error }}</p>
    <button
      v-if="status?.phase === 'ready'"
      class="flex w-full items-center justify-center gap-2 rounded-lg bg-green-500 px-4 py-2 text-sm font-medium text-white hover:bg-green-600 disabled:opacity-50"
      :disabled="busy"
      @click="$emit('activate')"
    >{{ busy ? t('version.restarting') : t('version.restartNow') }}</button>
    <button
      v-else-if="hasUpdate && !warning && status?.phase !== 'manual_recovery_required'"
      class="flex w-full items-center justify-center gap-2 rounded-lg bg-primary-500 px-4 py-2 text-sm font-medium text-white hover:bg-primary-600 disabled:opacity-50"
      :disabled="busy || !status"
      @click="$emit('prepare')"
    >{{ busy ? t('version.updating') : t('version.updateNow') }}</button>
    <a :href="releaseUrl" target="_blank" rel="noopener noreferrer" class="flex items-center justify-center gap-1 text-xs text-gray-500 hover:text-gray-700 dark:text-dark-300">
      {{ t('version.viewRelease') }}
      <Icon name="externalLink" size="xs" :stroke-width="2" />
    </a>
  </div>
</template>

<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import type { ManagedUpdateStatus } from '@/api/admin/system'
import Icon from '@/components/icons/Icon.vue'

defineProps<{
  status: ManagedUpdateStatus | null
  busy: boolean
  reconnecting: boolean
  connectionDelayed: boolean
  error: string
  warning: string
  hasUpdate: boolean
  releaseUrl: string
}>()
defineEmits<{ prepare: []; activate: [] }>()
const { t } = useI18n()
</script>
