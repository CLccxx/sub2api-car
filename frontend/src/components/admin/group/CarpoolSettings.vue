<template>
  <div class="space-y-3" data-testid="carpool-settings">
    <div class="flex items-center justify-between gap-3">
      <span class="input-label mb-0">{{ t('admin.groups.carpool.enabled') }}</span>
      <Toggle :model-value="members > 0" :aria-label="t('admin.groups.carpool.enabled')"
        @update:model-value="emit('update:members', $event ? 4 : 0)" />
    </div>
    <template v-if="members > 0">
      <div class="flex flex-wrap items-start gap-4">
        <label class="block">
          <span class="input-label">{{ t('admin.groups.carpool.members') }}</span>
          <input :value="members" type="number" min="2" max="5" step="1" required
            class="input w-28" :aria-label="t('admin.groups.carpool.members')" @change="changeMembers" />
        </label>
        <label class="block min-w-0 flex-1">
          <span class="input-label">{{ t('admin.groups.carpool.bonus') }}</span>
          <input ref="bonusInput" :value="bonusDraft" type="number" min="0" :max="MAX_CARPOOL_BONUS_USD" step="0.01" required
            class="input" :aria-label="t('admin.groups.carpool.bonus')" :aria-invalid="bonusInvalid"
            @input="changeBonus" />
        </label>
      </div>
      <p v-if="bonusInvalid" class="text-sm text-red-600 dark:text-red-400" role="alert">
        {{ t('admin.groups.carpool.bonusInvalid') }}
      </p>
      <p class="input-hint">{{ t('admin.groups.carpool.bonusHint') }}</p>
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
import { ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import Toggle from '@/components/common/Toggle.vue'
import { DEFAULT_CARPOOL_BONUS_USD, MAX_CARPOOL_BONUS_USD, isValidCarpoolBonus } from './carpoolSettings'

const props = defineProps<{ members: number; bonus?: number; syncedAt?: string | null }>()
const emit = defineEmits<{
  (e: 'update:members', members: number): void
  (e: 'update:bonus', bonus: number): void
}>()
const { t } = useI18n()
const bonusInput = ref<HTMLInputElement | null>(null)
// Keep invalid/blank drafts out of the model, but block saving until corrected.
const bonusDraft = ref(String(props.bonus ?? DEFAULT_CARPOOL_BONUS_USD))
const bonusInvalid = ref(false)

watch([() => props.bonus, () => props.members > 0], () => {
  bonusDraft.value = String(props.bonus ?? DEFAULT_CARPOOL_BONUS_USD)
  bonusInvalid.value = false
  bonusInput.value?.setCustomValidity('')
})

function validate() {
  if (props.members <= 0) return true
  bonusInput.value?.setCustomValidity('')
  const valid = bonusDraft.value.trim() !== '' && isValidCarpoolBonus(Number(bonusDraft.value)) &&
    (bonusInput.value?.checkValidity() ?? true)
  bonusInvalid.value = !valid
  bonusInput.value?.setCustomValidity(valid ? '' : t('admin.groups.carpool.bonusInvalid'))
  return valid
}

function changeBonus(event: Event) {
  bonusDraft.value = (event.target as HTMLInputElement).value
  if (validate()) emit('update:bonus', Number(bonusDraft.value))
}

defineExpose({ validate })

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
