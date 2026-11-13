<script setup lang="ts">
import { onBeforeUnmount, onMounted, ref, useId, useTemplateRef, nextTick } from 'vue'

const props = withDefaults(defineProps<{
  content?: string
  trigger?: 'hover' | 'click' | 'both'
  triggerLabel?: string
  widthClass?: string
}>(), {
  trigger: 'hover',
  widthClass: 'w-64',
})

const show = ref(false)
const pinned = ref(false)
const tooltipId = useId()
const triggerRef = useTemplateRef<HTMLElement>('trigger')
const tooltipRef = useTemplateRef<HTMLElement>('tooltip')
const tooltipStyle = ref({ top: '0px', left: '0px' })
const arrowLeft = ref('50%')
const belowTrigger = ref(false)

function openTooltip() {
  show.value = true
  nextTick(updatePosition)
}

function closeTooltip() {
  show.value = false
  pinned.value = false
}

function onEnter() {
  if (props.trigger === 'click') return
  openTooltip()
}

function isInside(container: HTMLElement | null, target: EventTarget | null): boolean {
  return target instanceof Node && !!container?.contains(target)
}

// 悬停模式下指针在触发图标与提示框之间往返时保持打开，便于选中提示里的文字。
function onLeave(event: MouseEvent) {
  if (props.trigger === 'click' || pinned.value) return
  if (isInside(tooltipRef.value, event.relatedTarget)) return
  if (props.trigger === 'both' && isInside(triggerRef.value, document.activeElement)) return
  closeTooltip()
}

function onTooltipLeave(event: MouseEvent) {
  if (props.trigger === 'click' || pinned.value) return
  if (isInside(triggerRef.value, event.relatedTarget)) return
  if (props.trigger === 'both' && isInside(triggerRef.value, document.activeElement)) return
  closeTooltip()
}

function onClick(event: MouseEvent) {
  if (props.trigger === 'hover') return
  event.stopPropagation()
  if (props.trigger === 'both' ? pinned.value : show.value) {
    closeTooltip()
    return
  }
  pinned.value = props.trigger === 'both'
  openTooltip()
}

function onFocus() {
  if (props.trigger === 'both') openTooltip()
}

function onBlur() {
  if (props.trigger === 'both' && !pinned.value) closeTooltip()
}

function onDocumentClick(event: MouseEvent) {
  if (props.trigger === 'hover' || !show.value) return
  const target = event.target as Node | null
  if (!target) return
  if (triggerRef.value?.contains(target) || tooltipRef.value?.contains(target)) return
  closeTooltip()
}

function onDocumentKeydown(event: KeyboardEvent) {
  if (props.trigger === 'hover' || !show.value) return
  if (event.key === 'Escape') {
    closeTooltip()
  }
}

function onViewportChange() {
  if (!show.value) return
  updatePosition()
}

function updatePosition() {
  const el = triggerRef.value
  if (!el) return
  const rect = el.getBoundingClientRect()
  if (props.trigger === 'both' && tooltipRef.value) {
    const margin = 8
    const { width, height } = tooltipRef.value.getBoundingClientRect()
    const center = rect.left + rect.width / 2
    const left = Math.max(margin, Math.min(center - width / 2, window.innerWidth - width - margin))
    belowTrigger.value = rect.top < height + 2 * margin && window.innerHeight - rect.bottom > rect.top
    const preferredTop = belowTrigger.value ? rect.bottom + margin : rect.top - height - margin
    const top = Math.max(margin, Math.min(preferredTop, window.innerHeight - height - margin))
    arrowLeft.value = `${Math.max(margin, Math.min(center - left, width - margin))}px`
    tooltipStyle.value = { top: `${top}px`, left: `${left}px` }
    return
  }
  tooltipStyle.value = {
    top: `${rect.top + window.scrollY}px`,
    left: `${rect.left + rect.width / 2 + window.scrollX}px`,
  }
}

onMounted(() => {
  document.addEventListener('click', onDocumentClick, true)
  document.addEventListener('keydown', onDocumentKeydown)
  window.addEventListener('resize', onViewportChange)
  window.addEventListener('scroll', onViewportChange, true)
})

onBeforeUnmount(() => {
  document.removeEventListener('click', onDocumentClick, true)
  document.removeEventListener('keydown', onDocumentKeydown)
  window.removeEventListener('resize', onViewportChange)
  window.removeEventListener('scroll', onViewportChange, true)
})
</script>

<template>
  <div
    ref="trigger"
    class="group relative ml-1 inline-flex items-center align-middle"
    @mouseenter="onEnter"
    @mouseleave="onLeave"
    @click="onClick"
  >
    <!-- Trigger Icon -->
    <slot name="trigger">
      <component
        :is="triggerLabel ? 'button' : 'span'"
        :type="triggerLabel ? 'button' : undefined"
        :aria-label="triggerLabel"
        :aria-describedby="triggerLabel && show ? tooltipId : undefined"
        :class="triggerLabel ? 'inline-flex shrink-0 items-center justify-center rounded focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-primary-500' : 'inline-flex'"
        @focus="onFocus"
        @blur="onBlur"
      >
        <svg
          class="h-4 w-4 cursor-help text-gray-400 transition-colors hover:text-primary-600 dark:text-gray-500 dark:hover:text-primary-400"
          aria-hidden="true"
          fill="none"
          viewBox="0 0 24 24"
          stroke="currentColor"
          stroke-width="2"
        >
          <path
            stroke-linecap="round"
            stroke-linejoin="round"
            d="M13 16h-1v-4h-1m1-4h.01M21 12a9 9 0 11-18 0 9 9 0 0118 0z"
          />
        </svg>
      </component>
    </slot>

    <!-- Teleport to body to escape modal overflow clipping -->
    <Teleport to="body">
      <!-- before: 伪元素向下延伸一段透明区域，盖住提示框与触发图标之间的空隙，让指针能连续移入提示框。 -->
      <div
        ref="tooltip"
        :id="tooltipId"
        v-show="show"
        role="tooltip"
        :class="[
          'fixed z-[99999] rounded-lg bg-gray-900 p-3 text-xs leading-relaxed text-white shadow-xl ring-1 ring-white/10 selection:bg-primary-200 selection:text-gray-900 before:absolute before:inset-x-0 before:h-3 dark:bg-gray-800 dark:selection:bg-primary-200 dark:selection:text-gray-900',
          props.trigger === 'both'
            ? ['max-w-[calc(100vw-1rem)]', belowTrigger ? 'before:bottom-full' : 'before:top-full']
            : '-translate-x-1/2 -translate-y-full before:top-full',
          props.widthClass,
        ]"
        :style="props.trigger === 'both' ? tooltipStyle : { top: `calc(${tooltipStyle.top} - 8px)`, left: tooltipStyle.left }"
        @mouseleave="onTooltipLeave"
      >
        <button
          v-if="props.trigger === 'click'"
          type="button"
          class="absolute right-1.5 top-1.5 rounded p-1 text-gray-300 transition-colors hover:bg-white/10 hover:text-white"
          aria-label="Close"
          @click.stop="closeTooltip"
        >
          <svg class="h-3.5 w-3.5" fill="none" viewBox="0 0 24 24" stroke="currentColor" stroke-width="2">
            <path stroke-linecap="round" stroke-linejoin="round" d="M6 18L18 6M6 6l12 12" />
          </svg>
        </button>
        <slot>{{ content }}</slot>
        <div
          class="absolute h-2 w-2 -translate-x-1/2 rotate-45 bg-gray-900 dark:bg-gray-800"
          :class="belowTrigger ? '-top-1' : '-bottom-1'"
          :style="{ left: arrowLeft }"
        ></div>
      </div>
    </Teleport>
  </div>
</template>
