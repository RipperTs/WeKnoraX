<template>
  <div class="kb-api-access-settings">
    <div class="section-header">
      <h3>{{ t('knowledgeEditor.apiAccess.title') }}</h3>
      <p>{{ t('knowledgeEditor.apiAccess.description') }}</p>
    </div>

    <t-tabs v-model="activeTab">
      <t-tab-panel value="keys" :label="t('knowledgeEditor.apiAccess.keysTab')" />
      <t-tab-panel value="docs" :label="t('knowledgeEditor.apiAccess.docsTab')" />
    </t-tabs>

    <div v-if="activeTab === 'docs'" class="docs-panel">
      <div class="quick-start">
        <h4>{{ t('knowledgeEditor.apiAccess.quickStartTitle') }}</h4>
        <ol class="setup-steps">
          <li>
            <span class="step-number">1</span>
            <t-button size="small" variant="text" @click="activeTab = 'keys'">
              {{ t('knowledgeEditor.apiAccess.createTitle') }}
            </t-button>
          </li>
          <li><span class="step-number">2</span>{{ t('knowledgeEditor.apiAccess.authStep') }}</li>
          <li><span class="step-number">3</span>{{ t('knowledgeEditor.apiAccess.callStep') }}</li>
        </ol>
        <div class="auth-header"><code>X-API-Key: &lt;API_KEY&gt;</code></div>
      </div>

      <section class="api-reference" :aria-label="t('knowledgeEditor.apiAccess.docsTab')">
        <div class="operation-switcher" role="group" :aria-label="t('knowledgeEditor.apiAccess.chooseOperation')">
          <button
            v-for="method in methods"
            :key="method"
            type="button"
            class="operation-option"
            :class="{ 'is-active': activeMethod === method }"
            :aria-pressed="activeMethod === method"
            @click="activeMethod = method"
          >
            <span class="method-badge" :class="`method-${method.toLowerCase()}`">{{ method }}</span>
            <span>{{ operations[method].title }}</span>
          </button>
        </div>

        <div class="operation-detail">
          <div class="operation-heading">
            <h4>{{ activeOperation.title }}</h4>
            <p>{{ activeOperation.description }}</p>
          </div>
          <div class="request-line">
            <span class="method-badge" :class="`method-${activeMethod.toLowerCase()}`">{{ activeMethod }}</span>
            <code>{{ endpoint }}</code>
          </div>

          <div class="card-heading detail-heading">
            <h5>{{ t('knowledgeEditor.apiAccess.parametersTitle') }}</h5>
            <span class="content-type">
              {{ activeMethod === 'PUT' ? 'multipart/form-data' : t('knowledgeEditor.apiAccess.queryParameters') }}
            </span>
          </div>
          <div class="table-wrap">
            <table class="parameter-table">
              <thead>
                <tr>
                  <th scope="col">{{ t('knowledgeEditor.apiAccess.parameterName') }}</th>
                  <th scope="col">{{ t('knowledgeEditor.apiAccess.parameterRequired') }}</th>
                  <th scope="col">{{ t('knowledgeEditor.apiAccess.parameterDescription') }}</th>
                </tr>
              </thead>
              <tbody>
                <tr v-for="parameter in parameters" :key="parameter.name">
                  <td>
                    <code>{{ parameter.name }}</code>
                    <span class="parameter-type">{{ parameter.type }}</span>
                  </td>
                  <td>
                    {{ parameter.required ? t('knowledgeEditor.apiAccess.required') : t('knowledgeEditor.apiAccess.optional') }}
                  </td>
                  <td>{{ parameter.description }}</td>
                </tr>
              </tbody>
            </table>
          </div>

          <div class="code-panel">
            <div class="code-toolbar">
              <h5>{{ t('knowledgeEditor.apiAccess.requestExampleTitle') }} <span class="code-language">cURL</span></h5>
              <t-button size="small" variant="text" @click="copyRequestExample">
                <template #icon><t-icon name="file-copy" /></template>
                {{ t('knowledgeEditor.apiAccess.copyExample') }}
              </t-button>
            </div>
            <pre><code>{{ requestExample }}</code></pre>
          </div>
          <p class="example-note">{{ t('knowledgeEditor.apiAccess.exampleHint') }}</p>

          <div class="code-panel">
            <div class="code-toolbar">
              <h5>{{ t('knowledgeEditor.apiAccess.responseExampleTitle') }}</h5>
              <span class="response-status">{{ activeMethod === 'PUT' ? '202 Accepted' : '200 OK' }}</span>
            </div>
            <pre><code>{{ responseExample }}</code></pre>
          </div>
          <p class="response-note" :class="{ 'response-note--delete': activeMethod === 'DELETE' }">
            {{ activeOperation.responseHint }}
          </p>
        </div>
      </section>

      <div class="reference-notes">
        <details>
          <summary>{{ t('knowledgeEditor.apiAccess.statusesTitle') }}</summary>
          <dl class="status-list">
            <div v-for="status in parseStatuses" :key="status.value">
              <dt><code>{{ status.value }}</code></dt>
              <dd>{{ status.description }}</dd>
            </div>
          </dl>
        </details>
        <details>
          <summary>{{ t('knowledgeEditor.apiAccess.errorsTitle') }}</summary>
          <dl class="status-list">
            <div v-for="error in httpErrors" :key="error.code">
              <dt><code>{{ error.code }}</code></dt>
              <dd>{{ error.description }}</dd>
            </div>
          </dl>
        </details>
        <details>
          <summary>{{ t('knowledgeEditor.apiAccess.syncRulesTitle') }}</summary>
          <ul class="sync-rules">
            <li>{{ t('knowledgeEditor.apiAccess.identityRule') }}</li>
            <li>{{ t('knowledgeEditor.apiAccess.skipRule') }}</li>
            <li>{{ t('knowledgeEditor.apiAccess.updateRule') }}</li>
            <li>{{ t('knowledgeEditor.apiAccess.orderRule') }}</li>
          </ul>
        </details>
      </div>
    </div>

    <div v-else class="keys-panel">
      <div class="create-card">
        <div class="card-heading">
          <div>
            <h4>{{ t('knowledgeEditor.apiAccess.createTitle') }}</h4>
            <p>{{ t('knowledgeEditor.apiAccess.createDescription') }}</p>
          </div>
        </div>
        <div class="create-row">
          <t-input
            v-model="keyName"
            :maxlength="128"
            :placeholder="t('knowledgeEditor.apiAccess.namePlaceholder')"
            @enter="createKey"
          />
          <t-button theme="primary" :loading="creating" @click="createKey">
            <template #icon><t-icon name="add" /></template>
            {{ t('knowledgeEditor.apiAccess.create') }}
          </t-button>
        </div>
      </div>

      <t-alert
        v-if="createdToken"
        class="created-key-alert"
        theme="warning"
        :title="t('knowledgeEditor.apiAccess.createdTitle')"
      >
        <template #operation>
          <t-button size="small" variant="outline" @click="copyCreatedToken">
            <template #icon><t-icon name="file-copy" /></template>
            {{ t('common.copy') }}
          </t-button>
        </template>
        <p class="created-key-description">
          {{ t('knowledgeEditor.apiAccess.createdDescription') }}
        </p>
        <div class="created-key-value">
          <code>{{ createdToken }}</code>
        </div>
      </t-alert>

      <div class="key-list-card">
        <div class="card-heading">
          <h4>{{ t('knowledgeEditor.apiAccess.listTitle') }}</h4>
          <t-button
            shape="square"
            variant="text"
            :loading="loading"
            :aria-label="t('common.refresh')"
            @click="loadKeys"
          >
            <t-icon name="refresh" />
          </t-button>
        </div>

        <t-alert v-if="errorMessage" theme="error" :message="errorMessage">
          <template #operation>
            <t-button size="small" @click="loadKeys">{{ t('common.retry') }}</t-button>
          </template>
        </t-alert>

        <div v-else-if="loading" class="list-state">
          <t-loading size="small" />
          <span>{{ t('knowledgeEditor.apiAccess.loading') }}</span>
        </div>

        <t-empty
          v-else-if="keys.length === 0"
          class="list-state list-state--empty"
          :description="t('knowledgeEditor.apiAccess.empty')"
        />

        <div v-else class="key-table-wrap">
          <table class="key-table">
            <thead>
              <tr>
                <th>{{ t('knowledgeEditor.apiAccess.columns.name') }}</th>
                <th>{{ t('knowledgeEditor.apiAccess.columns.key') }}</th>
                <th>{{ t('knowledgeEditor.apiAccess.columns.createdAt') }}</th>
                <th>{{ t('knowledgeEditor.apiAccess.columns.lastUsed') }}</th>
                <th class="actions-heading">{{ t('knowledgeEditor.apiAccess.columns.actions') }}</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="key in keys" :key="key.id">
                <td>{{ key.name }}</td>
                <td><code class="masked-key">{{ maskKey(key.api_key) }}</code></td>
                <td>{{ formatDate(key.created_at) }}</td>
                <td>{{ key.last_used_at ? formatDate(key.last_used_at) : t('knowledgeEditor.apiAccess.neverUsed') }}</td>
                <td class="actions-cell">
                  <t-button
                    shape="square"
                    variant="text"
                    theme="danger"
                    :title="t('knowledgeEditor.apiAccess.revoke')"
                    @click="confirmRevoke(key)"
                  >
                    <t-icon name="delete" />
                  </t-button>
                </td>
              </tr>
            </tbody>
          </table>
        </div>
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { DialogPlugin, MessagePlugin } from 'tdesign-vue-next'
import { useI18n } from 'vue-i18n'
import {
  createTenantAPIKey,
  deleteTenantAPIKey,
  listTenantAPIKeys,
  type TenantAPIKey,
} from '@/api/tenant'
import { useAuthStore } from '@/stores/auth'
import { copyWithToast } from '@/utils/clipboard'

const props = defineProps<{
  kbId: string
}>()

const { t } = useI18n()
const authStore = useAuthStore()

const activeTab = ref('keys')
const methods = ['PUT', 'GET', 'DELETE'] as const
const activeMethod = ref<typeof methods[number]>('PUT')
const loading = ref(false)
const creating = ref(false)
const keyName = ref('')
const createdToken = ref('')
const createdKeyId = ref<number | null>(null)
const errorMessage = ref('')
const allKeys = ref<TenantAPIKey[]>([])

const endpoint = computed(() => `/api/v1/knowledge-bases/${props.kbId}/external-documents`)
const requestURL = computed(() => {
  const origin = typeof window !== 'undefined' && window.location.origin !== 'null'
    ? window.location.origin
    : ''
  return `${origin}${endpoint.value}`
})
const operations = computed(() => ({
  PUT: {
    title: t('knowledgeEditor.apiAccess.upsertTitle'),
    description: t('knowledgeEditor.apiAccess.upsertDescription'),
    responseHint: t('knowledgeEditor.apiAccess.upsertResponseHint'),
  },
  GET: {
    title: t('knowledgeEditor.apiAccess.getTitle'),
    description: t('knowledgeEditor.apiAccess.getDescription'),
    responseHint: t('knowledgeEditor.apiAccess.getResponseHint'),
  },
  DELETE: {
    title: t('knowledgeEditor.apiAccess.deleteTitle'),
    description: t('knowledgeEditor.apiAccess.deleteDescription'),
    responseHint: t('knowledgeEditor.apiAccess.deleteResponseHint'),
  },
}))
const activeOperation = computed(() => operations.value[activeMethod.value])
const parameters = computed(() => {
  const identifiers = [
    { name: 'source_id', type: 'string', required: true, description: t('knowledgeEditor.apiAccess.sourceIdHint') },
    { name: 'external_id', type: 'string', required: true, description: t('knowledgeEditor.apiAccess.externalIdHint') },
  ]
  return activeMethod.value === 'PUT'
    ? [...identifiers,
      { name: 'file', type: 'file', required: true, description: t('knowledgeEditor.apiAccess.fileHint') },
      { name: 'metadata', type: 'JSON', required: false, description: t('knowledgeEditor.apiAccess.metadataHint') },
    ]
    : identifiers
})
const requestExample = computed(() => {
  const header = "  --header 'X-API-Key: <API_KEY>'"
  const lines = activeMethod.value === 'PUT'
    ? [
      `curl --request PUT '${requestURL.value}'`,
      header,
      "  --form 'source_id=policy-system'",
      "  --form 'external_id=article-10001'",
      "  --form 'file=@/path/to/document.pdf'",
      `  --form 'metadata={"department":"HR"}'`,
    ]
    : [
      `curl --get --request ${activeMethod.value} '${requestURL.value}'`,
      header,
      "  --data-urlencode 'source_id=policy-system'",
      "  --data-urlencode 'external_id=article-10001'",
    ]
  return lines.join(' \\\n')
})
const responseExample = computed(() => JSON.stringify({
  success: true,
  data: {
    ...(activeMethod.value === 'GET' ? {} : { action: activeMethod.value === 'PUT' ? 'created' : 'deleted' }),
    source_id: 'policy-system',
    external_id: 'article-10001',
    knowledge_id: '7dd32f36-5ad6-40ea-a735-e0a809146dea',
    parse_status: activeMethod.value === 'PUT' ? 'pending' : 'completed',
    content_fingerprint: 'e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855',
    updated_at: '2026-09-03T10:00:00Z',
  },
}, null, 2))
const parseStatuses = computed(() => [
  { value: 'pending', description: t('knowledgeEditor.apiAccess.statusPending') },
  { value: 'processing', description: t('knowledgeEditor.apiAccess.statusProcessing') },
  { value: 'finalizing', description: t('knowledgeEditor.apiAccess.statusFinalizing') },
  { value: 'completed', description: t('knowledgeEditor.apiAccess.statusCompleted') },
  { value: 'failed', description: t('knowledgeEditor.apiAccess.statusFailed') },
  { value: 'cancelled', description: t('knowledgeEditor.apiAccess.statusCancelled') },
])
const httpErrors = computed(() => [
  { code: 400, description: t('knowledgeEditor.apiAccess.error400') },
  { code: 401, description: t('knowledgeEditor.apiAccess.error401') },
  { code: 403, description: t('knowledgeEditor.apiAccess.error403') },
  { code: 404, description: t('knowledgeEditor.apiAccess.error404') },
  { code: 409, description: t('knowledgeEditor.apiAccess.error409') },
  { code: 429, description: t('knowledgeEditor.apiAccess.error429') },
  { code: 500, description: t('knowledgeEditor.apiAccess.error500') },
])
const tenantId = computed(() => Number(authStore.effectiveTenantId || 0))
const keys = computed(() => allKeys.value.filter((key) => {
  const knowledgeBaseIDs = (key.knowledge_base_ids || []).map(String)
  const capabilities = key.capabilities || []
  return !key.full_access
    && knowledgeBaseIDs.length === 1
    && knowledgeBaseIDs[0] === props.kbId
    && capabilities.length === 1
    && capabilities[0] === 'ingest'
}))

onMounted(loadKeys)

watch(() => props.kbId, () => {
  createdToken.value = ''
  createdKeyId.value = null
  void loadKeys()
})

async function loadKeys() {
  if (!tenantId.value) return
  loading.value = true
  errorMessage.value = ''
  try {
    const response = await listTenantAPIKeys(tenantId.value)
    if (!response.success) {
      throw new Error(response.message || t('knowledgeEditor.apiAccess.loadFailed'))
    }
    allKeys.value = response.data || []
  } catch (error: unknown) {
    errorMessage.value = error instanceof Error
      ? error.message
      : t('knowledgeEditor.apiAccess.loadFailed')
  } finally {
    loading.value = false
  }
}

async function createKey() {
  const name = keyName.value.trim()
  if (!name) {
    MessagePlugin.warning(t('knowledgeEditor.apiAccess.nameRequired'))
    return
  }
  if (!tenantId.value || creating.value) return

  creating.value = true
  try {
    const response = await createTenantAPIKey(tenantId.value, {
      name,
      full_access: false,
      knowledge_base_ids: [props.kbId],
      capabilities: ['ingest'],
    })
    const token = response.data?.token || response.data?.api_key || ''
    if (!response.success || !token) {
      throw new Error(response.message || t('knowledgeEditor.apiAccess.createFailed'))
    }
    createdToken.value = token
    createdKeyId.value = response.data?.id || null
    keyName.value = ''
    MessagePlugin.success(t('knowledgeEditor.apiAccess.createSuccess'))
    await loadKeys()
  } catch (error: unknown) {
    MessagePlugin.error(error instanceof Error ? error.message : t('knowledgeEditor.apiAccess.createFailed'))
  } finally {
    creating.value = false
  }
}

async function copyCreatedToken() {
  await copyWithToast(createdToken.value, 'knowledgeEditor.apiAccess.copySuccess')
}

async function copyRequestExample() {
  await copyWithToast(requestExample.value, 'knowledgeEditor.apiAccess.exampleCopySuccess')
}

function confirmRevoke(key: TenantAPIKey) {
  const dialog = DialogPlugin.confirm({
    header: t('knowledgeEditor.apiAccess.revokeConfirmTitle'),
    body: t('knowledgeEditor.apiAccess.revokeConfirmBody', { name: key.name }),
    confirmBtn: { content: t('knowledgeEditor.apiAccess.revoke'), theme: 'danger' },
    cancelBtn: t('common.cancel'),
    onConfirm: async () => {
      await revokeKey(key.id)
      dialog.destroy()
    },
    onClose: () => dialog.destroy(),
  })
}

async function revokeKey(keyId: number) {
  const response = await deleteTenantAPIKey(tenantId.value, keyId)
  if (!response.success) {
    MessagePlugin.error(response.message || t('knowledgeEditor.apiAccess.revokeFailed'))
    return
  }
  if (createdKeyId.value === keyId) {
    createdToken.value = ''
    createdKeyId.value = null
  }
  MessagePlugin.success(t('knowledgeEditor.apiAccess.revokeSuccess'))
  await loadKeys()
}

function maskKey(value: string) {
  if (!value) return '-'
  if (value.length <= 14) return '•'.repeat(value.length)
  return `${value.slice(0, 8)}${'•'.repeat(8)}${value.slice(-6)}`
}

function formatDate(value: string) {
  const timestamp = Date.parse(value)
  if (Number.isNaN(timestamp)) return '-'
  return new Intl.DateTimeFormat(undefined, {
    year: 'numeric',
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
  }).format(timestamp)
}
</script>

<style scoped lang="less">
.kb-api-access-settings,
.docs-panel,
.keys-panel {
  display: flex;
  min-width: 0;
  flex-direction: column;
  gap: 20px;
}

h3,
h4,
h5 {
  margin: 0;
  color: var(--td-text-color-primary);
}

h4 {
  font-size: 14px;
}

h5 {
  font-size: 13px;
  font-weight: 500;
}

.section-header p,
.card-heading p,
.operation-heading p {
  margin: 6px 0 0;
  color: var(--td-text-color-secondary);
  font-size: 13px;
  line-height: 1.6;
}

.api-reference,
.create-card,
.key-list-card {
  border: 1px solid var(--td-component-border);
  border-radius: 10px;
  background: var(--td-bg-color-container);
}

.create-card,
.key-list-card {
  padding: 18px;
}

.card-heading,
.code-toolbar {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
}

.content-type,
.parameter-type,
.code-language {
  color: var(--td-text-color-secondary);
  font-size: 12px;
  font-weight: 400;
}

code,
pre,
.method-badge,
.response-status {
  font-family: var(--font-mono, monospace);
}

.setup-steps {
  display: flex;
  flex-wrap: wrap;
  gap: 12px 24px;
  margin: 16px 0 12px;
  padding: 0;
  list-style: none;
}

.setup-steps li {
  display: flex;
  align-items: center;
  gap: 8px;
  color: var(--td-text-color-secondary);
  font-size: 12px;
}

.step-number {
  display: inline-flex;
  width: 22px;
  height: 22px;
  align-items: center;
  justify-content: center;
  border: 1px solid var(--td-component-border);
  border-radius: 50%;
  font-size: 11px;
}

.auth-header {
  padding: 10px 12px;
  border-radius: 6px;
  background: var(--td-bg-color-secondarycontainer);
  color: var(--td-text-color-primary);
  font-size: 12px;
}

.operation-switcher {
  display: grid;
  grid-template-columns: repeat(3, minmax(0, 1fr));
  gap: 6px;
  padding: 8px;
  border-bottom: 1px solid var(--td-component-stroke);
}

.operation-option {
  display: flex;
  min-width: 0;
  align-items: center;
  justify-content: center;
  flex-wrap: wrap;
  gap: 8px;
  padding: 12px 8px;
  border: 1px solid transparent;
  border-radius: 6px;
  background: transparent;
  color: var(--td-text-color-secondary);
  font: inherit;
  font-size: 12px;
  cursor: pointer;
  transition: background-color .15s, border-color .15s;
}

.operation-option:hover {
  background: var(--td-bg-color-container-hover);
}

.operation-option.is-active {
  border-color: var(--td-brand-color-focus);
  background: var(--td-brand-color-light);
  color: var(--td-text-color-primary);
}

.operation-option:focus-visible {
  outline: 2px solid var(--td-brand-color);
  outline-offset: 2px;
}

.method-badge {
  display: inline-block;
  flex-shrink: 0;
  padding: 3px 6px;
  border-radius: 4px;
  font-size: 11px;
  font-weight: 600;
  line-height: 1.4;
}

.method-put {
  background: var(--td-warning-color-light);
  color: var(--td-warning-color-active);
}

.method-get {
  background: var(--td-success-color-light);
  color: var(--td-success-color-active);
}

.method-delete {
  background: var(--td-error-color-light);
  color: var(--td-error-color-active);
}

.operation-detail {
  padding: 20px;
}

.request-line {
  display: flex;
  align-items: flex-start;
  gap: 8px;
  margin-top: 14px;
  color: var(--td-text-color-primary);
}

.request-line code {
  min-width: 0;
  overflow-wrap: anywhere;
  font-size: 12px;
  line-height: 1.8;
}

.detail-heading {
  margin: 24px 0 10px;
  flex-wrap: wrap;
}

.table-wrap {
  overflow-x: auto;
}

.parameter-table,
.key-table {
  width: 100%;
  border-collapse: collapse;
  color: var(--td-text-color-primary);
  font-size: 12px;
  line-height: 1.6;
}

.parameter-table th,
.parameter-table td,
.key-table th,
.key-table td {
  padding: 10px 8px;
  border-bottom: 1px solid var(--td-component-stroke);
  text-align: left;
  vertical-align: top;
}

.parameter-table th,
.key-table th {
  color: var(--td-text-color-secondary);
  font-weight: 500;
  white-space: nowrap;
}

.parameter-table td:first-child,
.parameter-table td:nth-child(2) {
  white-space: nowrap;
}

.parameter-type {
  display: block;
  font-size: 11px;
}

.code-panel {
  min-width: 0;
  margin-top: 22px;
  overflow: hidden;
  border: 1px solid var(--td-component-stroke);
  border-radius: 8px;
}

.code-toolbar {
  flex-wrap: wrap;
  min-height: 38px;
  padding: 6px 12px;
}

.code-language {
  margin-left: 8px;
}

.response-status {
  color: var(--td-success-color-active);
  font-size: 11px;
}

.code-panel pre {
  margin: 0;
  padding: 14px;
  overflow-x: auto;
  border-top: 1px solid var(--td-component-stroke);
  background: var(--td-bg-color-secondarycontainer);
  color: var(--td-text-color-primary);
  font-size: 12px;
  line-height: 1.7;
  tab-size: 2;
}

.example-note,
.response-note {
  margin: 10px 0 0;
  color: var(--td-text-color-secondary);
  font-size: 12px;
  line-height: 1.7;
}

.response-note--delete {
  color: var(--td-error-color-active);
}

.reference-notes details {
  border-bottom: 1px solid var(--td-component-stroke);
}

.reference-notes summary {
  padding: 14px 0;
  color: var(--td-text-color-primary);
  font-size: 13px;
  font-weight: 500;
  cursor: pointer;
}

.status-list {
  margin: 0 0 16px;
}

.status-list > div {
  display: flex;
  gap: 16px;
  margin: 8px 0;
  font-size: 12px;
  line-height: 1.6;
}

.status-list dt {
  flex: 0 0 90px;
  color: var(--td-text-color-primary);
}

.status-list dd {
  margin: 0;
  color: var(--td-text-color-secondary);
}

.sync-rules {
  margin: 0 0 16px;
  padding-left: 20px;
  color: var(--td-text-color-secondary);
  font-size: 12px;
  line-height: 1.8;
}

.sync-rules li + li {
  margin-top: 6px;
}

.create-row {
  display: grid;
  grid-template-columns: minmax(0, 1fr) auto;
  gap: 12px;
  margin-top: 16px;
}

.created-key-alert {
  align-items: flex-start;
}

.created-key-description {
  margin: 0;
}

.created-key-value {
  margin-top: 12px;
  overflow-x: auto;
}

.created-key-value code {
  user-select: all;
  white-space: nowrap;
}

.list-state {
  display: flex;
  min-height: 120px;
  align-items: center;
  justify-content: center;
  gap: 8px;
  color: var(--td-text-color-secondary);
}

.list-state--empty {
  flex-direction: column;
}

.key-table-wrap {
  margin-top: 14px;
  overflow-x: auto;
}

.key-table {
  font-size: 13px;
}

.key-table td {
  white-space: nowrap;
  vertical-align: middle;
}

.actions-heading,
.actions-cell {
  width: 56px;
  text-align: right !important;
}

.masked-key {
  color: var(--td-text-color-secondary);
}

@media (max-width: 720px) {
  .create-row {
    grid-template-columns: 1fr;
  }
  .operation-detail {
    padding: 14px;
  }
  .operation-option {
    flex-direction: column;
    gap: 6px;
    padding: 10px 4px;
  }
}

</style>
