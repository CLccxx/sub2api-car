<template>
  <div class="space-y-3" data-testid="carpool-settings">
    <div class="flex items-center justify-between gap-3">
      <span class="input-label mb-0">{{ t('admin.groups.carpool.enabled') }}</span>
      <Toggle :model-value="members > 0" :aria-label="t('admin.groups.carpool.enabled')"
        @update:model-value="emit('update:members', $event ? 4 : 0)" />
    </div>
    <template v-if="members > 0">
      <label class="block">
        <span class="input-label">{{ t('admin.groups.carpool.members') }}</span>
        <input :value="members" type="number" min="2" max="5" step="1" required
          class="input w-28" :aria-label="t('admin.groups.carpool.members')" @change="changeMembers" />
      </label>
      <dl class="space-y-1 text-sm text-gray-500 dark:text-gray-400">
        <div class="flex flex-wrap justify-between gap-x-3 gap-y-1">
          <dt>{{ t('admin.groups.carpool.schedule') }}</dt>
          <dd>{{ t('admin.groups.carpool.scheduleValue') }}</dd>
        </div>
        <div v-if="syncedAt" class="flex flex-wrap justify-between gap-x-3 gap-y-1">
          <dt>{{ t('admin.groups.carpool.lastUpdated') }}</dt>
          <dd>{{ formatSyncTime(syncedAt) }}</dd>
        </div>
      </dl>
    </template>
  </div>
</template>

<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import Toggle from '@/components/common/Toggle.vue'

defineProps<{ members: number; syncedAt?: string | null }>()
const emit = defineEmits<{ (e: 'update:members', members: number): void }>()
const { t } = useI18n()

function changeMembers(event: Event) {
  const input = event.target as HTMLInputElement
  if (input.checkValidity()) emit('update:members', Number(input.value))
}

function formatSyncTime(value: string) {
  return new Intl.DateTimeFormat('zh-CN', {
    timeZone: 'Asia/Shanghai', year: 'numeric', month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit', hourCycle: 'h23'
  }).format(new Date(value))
}
</script>
