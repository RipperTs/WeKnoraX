import { ref } from 'vue'
import { defineStore } from 'pinia'
import { getSystemInfo, type SystemInfo } from '@/api/system'
import { getApiBaseUrl } from '@/utils/api-base'

export const DEFAULT_SITE_TITLE = '企业知识库'

function resolveLogoURL(path: string): string {
  if (!path || /^(?:https?:\/\/|data:)/i.test(path)) return path
  const base = getApiBaseUrl()
  if (!base || base === '/') return path
  return `${base}${path.startsWith('/') ? path : `/${path}`}`
}

export const useSystemBrandingStore = defineStore('systemBranding', () => {
  const siteTitle = ref(DEFAULT_SITE_TITLE)
  const siteLogoURL = ref('')
  const edition = ref('')
  const loaded = ref(false)
  let loadingPromise: Promise<SystemInfo | null> | null = null
  let siteTitleRevision = 0
  let siteLogoRevision = 0

  async function ensureLoaded(force = false): Promise<SystemInfo | null> {
    if (loaded.value && !force) {
      return {
        version: '',
        edition: edition.value,
        site_title: siteTitle.value,
        site_logo_url: siteLogoURL.value,
      }
    }
    if (loadingPromise) {
      const current = await loadingPromise
      return force ? ensureLoaded(true) : current
    }

    const requestSiteTitleRevision = siteTitleRevision
    const requestSiteLogoRevision = siteLogoRevision
    loadingPromise = (async () => {
      try {
        const response = await getSystemInfo()
        const info = response?.data ?? null
        if (info) {
          if (requestSiteTitleRevision === siteTitleRevision) {
            siteTitle.value = info.site_title?.trim() || DEFAULT_SITE_TITLE
          }
          if (requestSiteLogoRevision === siteLogoRevision) {
            siteLogoURL.value = resolveLogoURL(info.site_logo_url || '')
          }
          edition.value = info.edition || ''
          loaded.value = true
        }
        return info
      } catch {
        return null
      } finally {
        loadingPromise = null
      }
    })()

    return loadingPromise
  }

  function applySiteTitle(value: unknown) {
    siteTitleRevision += 1
    siteTitle.value = typeof value === 'string' ? value.trim() || DEFAULT_SITE_TITLE : DEFAULT_SITE_TITLE
    loaded.value = true
  }

  function applySiteLogo(value: unknown) {
    siteLogoRevision += 1
    siteLogoURL.value = resolveLogoURL(typeof value === 'string' ? value : '')
    loaded.value = true
  }

  function useDefaultLogo() {
    siteLogoURL.value = ''
  }

  return {
    siteTitle,
    siteLogoURL,
    edition,
    loaded,
    ensureLoaded,
    applySiteTitle,
    applySiteLogo,
    useDefaultLogo,
  }
})
