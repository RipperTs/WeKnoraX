const INTEGRATION_LOGIN_RETURN_KEY = 'weknora_integration_login_return'
const INTEGRATION_OIDC_ATTEMPTED_KEY = 'weknora_integration_oidc_attempted'

function isSafeIntegrationPath(path: string) {
  return path.startsWith('/integrations/') && !path.startsWith('//')
}

export function rememberIntegrationLoginReturn(path: string) {
  if (!isSafeIntegrationPath(path)) return
  sessionStorage.setItem(INTEGRATION_LOGIN_RETURN_KEY, path)
  sessionStorage.removeItem(INTEGRATION_OIDC_ATTEMPTED_KEY)
}

export function peekIntegrationLoginReturn() {
  const path = sessionStorage.getItem(INTEGRATION_LOGIN_RETURN_KEY) || ''
  return isSafeIntegrationPath(path) ? path : ''
}

export function consumeIntegrationLoginReturn() {
  const path = peekIntegrationLoginReturn()
  sessionStorage.removeItem(INTEGRATION_LOGIN_RETURN_KEY)
  sessionStorage.removeItem(INTEGRATION_OIDC_ATTEMPTED_KEY)
  return path
}

export function markIntegrationOIDCAttempted() {
  sessionStorage.setItem(INTEGRATION_OIDC_ATTEMPTED_KEY, 'true')
}

export function hasIntegrationOIDCAttempted() {
  return sessionStorage.getItem(INTEGRATION_OIDC_ATTEMPTED_KEY) === 'true'
}

export function clearIntegrationOIDCAttempted() {
  sessionStorage.removeItem(INTEGRATION_OIDC_ATTEMPTED_KEY)
}
