import { ref } from 'vue'
import { defineStore } from 'pinia'
import { getSystemInfo, type SystemInfo } from '@/api/system'
import { safeGetItem, safeRemoveItem, safeSetItem } from '@/composables/preferenceStorage'
import { getApiBaseUrl } from '@/utils/api-base'

export const DEFAULT_SITE_TITLE = '企业知识库'
const SYSTEM_BRANDING_STORAGE_KEY = 'WeKnora_system_branding'

interface StoredSystemBranding {
  site_title: string
  site_logo_url: string
}

function normalizeSiteTitle(value: unknown): string {
  return typeof value === 'string' ? value.trim() || DEFAULT_SITE_TITLE : DEFAULT_SITE_TITLE
}

function normalizeSiteLogoPath(value: unknown): string {
  return typeof value === 'string' ? value : ''
}

function loadStoredBranding(): StoredSystemBranding | null {
  const raw = safeGetItem(SYSTEM_BRANDING_STORAGE_KEY)
  if (!raw) return null

  try {
    const parsed: unknown = JSON.parse(raw)
    if (
      !parsed ||
      typeof parsed !== 'object' ||
      typeof (parsed as StoredSystemBranding).site_title !== 'string' ||
      typeof (parsed as StoredSystemBranding).site_logo_url !== 'string'
    ) {
      safeRemoveItem(SYSTEM_BRANDING_STORAGE_KEY)
      return null
    }
    return parsed as StoredSystemBranding
  } catch {
    safeRemoveItem(SYSTEM_BRANDING_STORAGE_KEY)
    return null
  }
}

function resolveLogoURL(path: string): string {
  if (!path || /^(?:https?:\/\/|data:)/i.test(path)) return path
  const base = getApiBaseUrl()
  if (!base || base === '/') return path
  return `${base}${path.startsWith('/') ? path : `/${path}`}`
}

export const useSystemBrandingStore = defineStore('systemBranding', () => {
  const storedBranding = loadStoredBranding()
  let hasStoredBranding = storedBranding !== null
  let storedSiteTitle = normalizeSiteTitle(storedBranding?.site_title)
  let storedSiteLogoPath = normalizeSiteLogoPath(storedBranding?.site_logo_url)
  const siteTitle = ref(storedSiteTitle)
  const siteLogoURL = ref(resolveLogoURL(storedSiteLogoPath))
  const edition = ref('')
  const loaded = ref(false)
  let loadingPromise: Promise<SystemInfo | null> | null = null
  let siteTitleRevision = 0
  let siteLogoRevision = 0

  function persistStoredBranding() {
    safeSetItem(SYSTEM_BRANDING_STORAGE_KEY, JSON.stringify({
      site_title: storedSiteTitle,
      site_logo_url: storedSiteLogoPath,
    }))
    hasStoredBranding = true
  }

  async function ensureLoaded(): Promise<SystemInfo | null> {
    if (loaded.value) {
      return {
        version: '',
        edition: edition.value,
        site_title: siteTitle.value,
        site_logo_url: siteLogoURL.value,
      }
    }
    if (loadingPromise) {
      return loadingPromise
    }

    const requestSiteTitleRevision = siteTitleRevision
    const requestSiteLogoRevision = siteLogoRevision
    loadingPromise = (async () => {
      try {
        const response = await getSystemInfo()
        const info = response?.data ?? null
        if (info) {
          let storageChanged = false
          if (requestSiteTitleRevision === siteTitleRevision) {
            const nextSiteTitle = normalizeSiteTitle(info.site_title)
            if (siteTitle.value !== nextSiteTitle) siteTitle.value = nextSiteTitle
            if (storedSiteTitle !== nextSiteTitle) {
              storedSiteTitle = nextSiteTitle
              storageChanged = true
            }
          }
          if (requestSiteLogoRevision === siteLogoRevision) {
            const nextSiteLogoPath = normalizeSiteLogoPath(info.site_logo_url)
            const nextSiteLogoURL = resolveLogoURL(nextSiteLogoPath)
            if (siteLogoURL.value !== nextSiteLogoURL) siteLogoURL.value = nextSiteLogoURL
            if (storedSiteLogoPath !== nextSiteLogoPath) {
              storedSiteLogoPath = nextSiteLogoPath
              storageChanged = true
            }
          }
          if (storageChanged || !hasStoredBranding) persistStoredBranding()
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
    const nextSiteTitle = normalizeSiteTitle(value)
    siteTitle.value = nextSiteTitle
    if (storedSiteTitle !== nextSiteTitle) {
      storedSiteTitle = nextSiteTitle
      persistStoredBranding()
    }
    loaded.value = true
  }

  function applySiteLogo(displayValue: unknown, storedValue: unknown) {
    siteLogoRevision += 1
    siteLogoURL.value = resolveLogoURL(normalizeSiteLogoPath(displayValue))
    loaded.value = true

    const nextStoredSiteLogoPath = normalizeSiteLogoPath(storedValue)
    if (storedSiteLogoPath !== nextStoredSiteLogoPath) {
      storedSiteLogoPath = nextStoredSiteLogoPath
      persistStoredBranding()
    }
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
