import { describe, expect, it, vi } from 'vitest'
import { createI18n } from 'vue-i18n'

describe('translation HTML security', () => {
  it('interpolates both locales without unsafe-eval in the runtime build', () => {
    const instance = createI18n({
      legacy: false,
      locale: 'en',
      messages: {
        en: { greeting: 'Hello, {name}!' },
        zh: { greeting: '你好，{name}！' }
      }
    })
    const dynamicFunction = vi.spyOn(globalThis, 'Function').mockImplementation(() => {
      throw new Error('CSP blocks dynamic functions')
    })
    const dynamicEval = vi.spyOn(globalThis, 'eval').mockImplementation(() => {
      throw new Error('CSP blocks eval')
    })
    const translations: string[] = []
    try {
      translations.push(instance.global.t('greeting', { name: 'Ada' }))
      instance.global.locale.value = 'zh'
      translations.push(instance.global.t('greeting', { name: 'Ada' }))
    } finally {
      dynamicFunction.mockRestore()
      dynamicEval.mockRestore()
    }

    expect(translations).toEqual(['Hello, Ada!', '你好，Ada！'])
  })

  it('neutralizes iframe srcdoc when translated HTML is escaped', () => {
    const instance = createI18n({
      legacy: false,
      locale: 'en',
      escapeParameter: true,
      warnHtmlMessage: false,
      messages: {
        en: {
          // A message function exercises the shipped runtime-only build too.
          frame: () => '<iframe srcdoc="<b>untrusted nested document</b>"></iframe>'
        }
      }
    })
    const template = document.createElement('template')
    template.innerHTML = instance.global.t('frame')
    const frame = template.content.querySelector('iframe')

    expect(frame).not.toBeNull()
    expect(frame?.hasAttribute('srcdoc')).toBe(false)
  })
})
