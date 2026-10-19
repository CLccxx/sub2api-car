<template>
  <BaseDialog
    :show="true"
    :title="t('admin.subscriptions.resetAll.title')"
    width="normal"
    :close-on-escape="!state.running"
    :show-close-button="!state.running"
    @close="close"
  >
    <div class="space-y-4">
      <p class="text-sm text-gray-600 dark:text-gray-300">
        {{ t('admin.subscriptions.resetAll.scope') }}
      </p>
      <div v-if="state.loading" role="status" class="flex items-center gap-2 py-4 text-sm text-gray-600 dark:text-gray-300">
        <Icon name="refresh" size="md" class="animate-spin" />
        {{ t('admin.subscriptions.resetAll.loading') }}
      </div>
      <p v-else-if="state.loadError" role="alert" class="break-words text-sm text-red-600 dark:text-red-400">
        {{ errorText(state.loadError) }}
      </p>
      <template v-else-if="state.loaded">
        <p v-if="counts.total === 0" class="py-4 text-sm text-gray-500 dark:text-gray-400">
          {{ t('admin.subscriptions.resetAll.empty') }}
        </p>
        <template v-else>
          <template v-if="!state.confirmed">
            <p class="text-base font-semibold text-gray-900 dark:text-white">
              {{ t('admin.subscriptions.resetAll.confirmTargets', { count: counts.total }) }}
            </p>
            <p class="rounded-lg bg-amber-50 p-3 text-sm text-amber-900 dark:bg-amber-900/20 dark:text-amber-200">
              {{ t('admin.subscriptions.resetAll.warning') }}
            </p>
          </template>
          <template v-else>
            <p v-if="state.resumed && !finished" class="text-sm text-amber-700 dark:text-amber-300">
              {{ t('admin.subscriptions.resetAll.resumed') }}
            </p>
            <div aria-live="polite" class="space-y-3">
              <p class="text-sm font-medium text-gray-900 dark:text-white">
                {{ t(finished ? 'admin.subscriptions.resetAll.completed' : 'admin.subscriptions.resetAll.progress', {
                  done: counts.processed, total: counts.total
                }) }}
              </p>
              <progress
                :value="counts.processed"
                :max="counts.total"
                :aria-label="t('admin.subscriptions.resetAll.progress', { done: counts.processed, total: counts.total })"
                class="h-2 w-full accent-primary-600"
              />
              <dl class="grid grid-cols-3 gap-3 text-center text-sm">
                <div v-for="metric in metrics" :key="metric">
                  <dt class="text-gray-500 dark:text-gray-400">{{ t(`admin.subscriptions.resetAll.${metric}`) }}</dt>
                  <dd class="mt-1 text-lg font-semibold tabular-nums text-gray-900 dark:text-white">{{ counts[metric] }}</dd>
                </div>
              </dl>
            </div>
            <div v-if="state.requestError" role="alert" class="space-y-2 rounded-lg bg-red-50 p-3 text-sm text-red-700 dark:bg-red-900/20 dark:text-red-300">
              <p class="break-words">{{ errorText(state.requestError) }}</p>
              <p v-if="!state.blocked">{{ t('admin.subscriptions.resetAll.interrupted') }}</p>
              <p v-if="state.pending" class="break-words">
                {{ t('admin.subscriptions.resetAll.uncertainTargets') }}:
                {{ state.pending.request.subscription_ids.map(id => `#${id}`).join(', ') }}
              </p>
            </div>
            <ul v-if="failedResults.length" class="max-h-48 space-y-2 overflow-auto" :aria-label="t('admin.subscriptions.resetAll.failures')">
              <li v-for="item in failedResults" :key="item.subscription_id" class="break-words border-b border-gray-100 py-2 text-sm dark:border-dark-700">
                <span class="font-medium text-gray-900 dark:text-white">#{{ item.subscription_id }}</span>
                <span class="ml-2 text-red-600 dark:text-red-400">{{ item.error || t('admin.subscriptions.bulk.itemFailed') }}</span>
              </li>
            </ul>
          </template>
          <p v-if="state.recoveryUnavailable" role="alert" class="text-sm text-amber-700 dark:text-amber-300">
            {{ t('admin.subscriptions.resetAll.recoveryUnavailable') }}
          </p>
        </template>
      </template>
    </div>
    <template #footer>
      <div class="flex flex-wrap justify-end gap-3">
        <button type="button" class="btn btn-secondary" data-test="reset-all-close" :disabled="state.running" @click="close">
          {{ t(state.blocked ? 'admin.subscriptions.resetAll.endOperation' : state.confirmed ? 'common.close' : 'common.cancel') }}
        </button>
        <button v-if="state.loadError" type="button" class="btn btn-primary" data-test="reset-all-reload" @click="load">
          <Icon name="refresh" size="sm" class="mr-2" />
          {{ t('admin.subscriptions.resetAll.reload') }}
        </button>
        <button
          v-else-if="state.loaded && counts.total > 0 && !finished && !state.blocked"
          type="button" class="btn btn-danger" data-test="reset-all-confirm"
          :disabled="state.running" @click="execute(false)"
        >
          <Icon name="refresh" size="sm" :class="['mr-2', { 'animate-spin': state.running }]" />
          {{ t(state.running ? 'common.processing' : state.confirmed ? 'admin.subscriptions.resetAll.resume' : 'admin.subscriptions.resetAll.confirm') }}
        </button>
        <button
          v-else-if="finished && failedResults.length > 0" type="button"
          class="btn btn-primary" data-test="reset-all-retry" @click="execute(true)"
        >
          <Icon name="refresh" size="sm" class="mr-2" />
          {{ t('admin.subscriptions.resetAll.retryFailed', { count: failedResults.length }) }}
        </button>
      </div>
    </template>
  </BaseDialog>
</template>

<script setup lang="ts">
import { onMounted, onUnmounted } from 'vue'
import { useI18n } from 'vue-i18n'
import BaseDialog from '@/components/common/BaseDialog.vue'
import Icon from '@/components/icons/Icon.vue'
import { useResetAllSubscriptionQuotas } from './useResetAllSubscriptionQuotas'

const emit = defineEmits<{ close: []; updated: [] }>()
const { t } = useI18n()
const { state, counts, failedResults, finished, load, run, retryFailed, dismiss, dispose } = useResetAllSubscriptionQuotas()
const metrics = ['success', 'failed', 'skipped'] as const
let mounted = true

const errorText = (message: string) => message.startsWith('admin.subscriptions.resetAll.') ? t(message) : message

async function execute(retry: boolean) {
  if (state.running) return
  if (retry) await retryFailed()
  else await run()
  if (mounted && state.confirmed) emit('updated')
}

function close() {
  if (state.running) return
  dismiss()
  emit('close')
}

function beforeUnload(event: BeforeUnloadEvent) {
  if (!state.running) return
  event.preventDefault()
  event.returnValue = ''
}

onMounted(() => {
  void load()
  window.addEventListener('beforeunload', beforeUnload)
})
onUnmounted(() => {
  mounted = false
  dispose()
  window.removeEventListener('beforeunload', beforeUnload)
})
</script>
