<template>
  <div class="kb-api-access-settings">
    <div class="section-header">
      <h3>{{ t('knowledgeEditor.apiAccess.title') }}</h3>
      <p>{{ t('knowledgeEditor.apiAccess.description') }}</p>
    </div>

    <div class="api-endpoint-card">
      <span class="field-label">{{ t('knowledgeEditor.apiAccess.endpointLabel') }}</span>
      <code>PUT {{ endpoint }}</code>
      <p>{{ t('knowledgeEditor.apiAccess.scopeNotice') }}</p>
    </div>

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
      :message="t('knowledgeEditor.apiAccess.createdDescription')"
    >
      <template #operation>
        <t-button size="small" variant="outline" @click="copyCreatedToken">
          <template #icon><t-icon name="file-copy" /></template>
          {{ t('common.copy') }}
        </t-button>
      </template>
      <div class="created-key-value">
        <code>{{ createdToken }}</code>
      </div>
    </t-alert>

    <div class="key-list-card">
      <div class="card-heading">
        <h4>{{ t('knowledgeEditor.apiAccess.listTitle') }}</h4>
        <t-button shape="square" variant="text" :loading="loading" @click="loadKeys">
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
        class="list-state"
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

const loading = ref(false)
const creating = ref(false)
const keyName = ref('')
const createdToken = ref('')
const createdKeyId = ref<number | null>(null)
const errorMessage = ref('')
const allKeys = ref<TenantAPIKey[]>([])

const endpoint = computed(() => `/api/v1/knowledge-bases/${props.kbId}/external-documents`)
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
.kb-api-access-settings {
  display: flex;
  flex-direction: column;
  gap: 20px;
}

.section-header h3,
.card-heading h4 {
  margin: 0;
  color: var(--td-text-color-primary);
}

.section-header p,
.card-heading p,
.api-endpoint-card p {
  margin: 6px 0 0;
  color: var(--td-text-color-secondary);
  font-size: 13px;
  line-height: 1.6;
}

.api-endpoint-card,
.create-card,
.key-list-card {
  padding: 18px;
  border: 1px solid var(--td-component-border);
  border-radius: 10px;
  background: var(--td-bg-color-container);
}

.field-label {
  display: block;
  margin-bottom: 8px;
  color: var(--td-text-color-secondary);
  font-size: 13px;
}

.api-endpoint-card code,
.created-key-value code,
.masked-key {
  font-family: var(--font-mono, monospace);
}

.api-endpoint-card code {
  display: block;
  overflow-x: auto;
  padding: 10px 12px;
  border-radius: 6px;
  background: var(--td-bg-color-secondarycontainer);
  color: var(--td-text-color-primary);
}

.card-heading {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 16px;
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

.key-table-wrap {
  margin-top: 14px;
  overflow-x: auto;
}

.key-table {
  width: 100%;
  border-collapse: collapse;
  font-size: 13px;
}

.key-table th,
.key-table td {
  padding: 12px 10px;
  border-bottom: 1px solid var(--td-component-stroke);
  color: var(--td-text-color-primary);
  text-align: left;
  white-space: nowrap;
}

.key-table th {
  color: var(--td-text-color-secondary);
  font-weight: 500;
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
}
</style>
