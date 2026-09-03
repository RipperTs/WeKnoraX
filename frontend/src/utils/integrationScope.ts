import type { IntegrationScope } from '@/api/integration'

export const integrationScopeLabelKeys: Record<IntegrationScope, string> = {
  'knowledge.read': 'thirdPartyIntegration.scopes.read',
  'knowledge.chat': 'thirdPartyIntegration.scopes.chat',
}
