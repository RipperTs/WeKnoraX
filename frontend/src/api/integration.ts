import { del, get, post, put } from '@/utils/request'

export type IntegrationScope = 'knowledge.read' | 'knowledge.chat'

export interface IntegrationApplication {
  id: string
  client_id: string
  name: string
  description: string
  redirect_uris: string[]
  allowed_scopes: IntegrationScope[]
  enabled: boolean
  created_at: string
  updated_at: string
}

export interface TenantIntegrationPolicy {
  id?: string
  application_id: string
  tenant_id: number
  enabled: boolean
  allowed_scopes: IntegrationScope[]
  knowledge_base_ids: string[]
}

export interface TenantIntegrationApplicationView {
  application: IntegrationApplication
  policy?: TenantIntegrationPolicy
}

export interface IntegrationKnowledgeBase {
  id: string
  name: string
  description?: string
  type: string
  tenant_id: number
}

export interface IntegrationConnection {
  id: string
  application_id: string
  tenant_id: number
  user_id: string
  scopes: IntegrationScope[]
  status: 'active' | 'revoked'
  last_used_at?: string
  created_at: string
  updated_at: string
}

export interface IntegrationConnectionView {
  connection: IntegrationConnection
  application: IntegrationApplication
  knowledge_bases: IntegrationKnowledgeBase[]
  effective_knowledge_base_ids: string[]
  effective_scopes: IntegrationScope[]
  available: boolean
  unavailable_reason?: 'application_disabled' | 'tenant_policy_disabled' | 'scope_unavailable'
}

export interface IntegrationAuthorizationParameters {
  client_id: string
  redirect_uri: string
  state: string
  scope: string
  code_challenge: string
  code_challenge_method: string
}

export interface IntegrationAuthorizationView {
  application: IntegrationApplication
  scopes: IntegrationScope[]
  knowledge_bases: IntegrationKnowledgeBase[]
  selected_knowledge_base_ids: string[]
  connection_id?: string
  requires_consent: boolean
}

export interface IntegrationApplicationInput {
  name: string
  description: string
  redirect_uris: string[]
  allowed_scopes: IntegrationScope[]
  enabled: boolean
}

export interface IntegrationCallbackTestResult {
  redirect_uri: string
  reachable: boolean
  status_code?: number
  error?: string
}

type DataResponse<T> = { success: boolean; data: T }

export function listIntegrationApplications(): Promise<DataResponse<IntegrationApplication[]>> {
  return get('/api/v1/system/admin/integration-applications')
}

export function createIntegrationApplication(
  input: IntegrationApplicationInput,
): Promise<DataResponse<{ application: IntegrationApplication; client_secret: string }>> {
  return post('/api/v1/system/admin/integration-applications', input)
}

export function updateIntegrationApplication(
  id: string,
  input: IntegrationApplicationInput,
): Promise<DataResponse<IntegrationApplication>> {
  return put(`/api/v1/system/admin/integration-applications/${id}`, input)
}

export function deleteIntegrationApplication(id: string): Promise<{ success: boolean }> {
  return del(`/api/v1/system/admin/integration-applications/${id}`)
}

export function rotateIntegrationApplicationSecret(
  id: string,
): Promise<DataResponse<{ application: IntegrationApplication; client_secret: string }>> {
  return post(`/api/v1/system/admin/integration-applications/${id}/rotate-secret`, {})
}

export function testIntegrationApplicationCallbacks(
  id: string,
): Promise<DataResponse<IntegrationCallbackTestResult[]>> {
  return post(`/api/v1/system/admin/integration-applications/${id}/test-callbacks`, {})
}

export function listTenantIntegrationApplications(): Promise<
  DataResponse<TenantIntegrationApplicationView[]>
> {
  return get('/api/v1/integrations/applications')
}

export function listTenantIntegrationKnowledgeBases(): Promise<
  DataResponse<IntegrationKnowledgeBase[]>
> {
  return get('/api/v1/integrations/knowledge-bases')
}

export function updateTenantIntegrationPolicy(
  applicationId: string,
  input: Pick<TenantIntegrationPolicy, 'enabled' | 'allowed_scopes' | 'knowledge_base_ids'>,
): Promise<DataResponse<TenantIntegrationPolicy>> {
  return put(`/api/v1/integrations/applications/${applicationId}/policy`, input)
}

export function listIntegrationConnections(): Promise<DataResponse<IntegrationConnectionView[]>> {
  return get('/api/v1/integrations/connections')
}

export function getIntegrationConnection(
  id: string,
  tenantId?: number,
): Promise<DataResponse<IntegrationConnectionView>> {
  return get(`/api/v1/integrations/connections/${id}`, integrationTenantConfig(tenantId))
}

export function revokeIntegrationConnection(id: string): Promise<{ success: boolean }> {
  return del(`/api/v1/integrations/connections/${id}`)
}

export function getIntegrationAuthorization(
  params: IntegrationAuthorizationParameters,
  tenantId?: number,
): Promise<DataResponse<IntegrationAuthorizationView>> {
  const query = new URLSearchParams()
  for (const [key, value] of Object.entries(params)) {
    query.set(key, value)
  }
  return get(
    `/api/v1/integrations/authorization?${query.toString()}`,
    integrationTenantConfig(tenantId),
  )
}

export function authorizeIntegration(input: {
  parameters: IntegrationAuthorizationParameters
  approved: boolean
  reuse_existing: boolean
  knowledge_base_ids: string[]
}, tenantId?: number): Promise<DataResponse<{ redirect_uri: string; connection_id?: string }>> {
  const { scope, ...parameters } = input.parameters
  return post(
    '/api/v1/integrations/authorization',
    {
      ...input,
      parameters: {
        ...parameters,
        scopes: String(scope || '').split(/\s+/).filter(Boolean),
      },
    },
    integrationTenantConfig(tenantId),
  )
}

function integrationTenantConfig(tenantId?: number) {
  if (!tenantId) return undefined
  return { headers: { 'X-Tenant-ID': String(tenantId) } }
}
