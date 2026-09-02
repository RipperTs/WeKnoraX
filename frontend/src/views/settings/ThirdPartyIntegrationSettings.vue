<template>
  <div class="integration-settings">
    <div class="section-header">
      <div>
        <h2>{{ t('thirdPartyIntegration.tenant.title') }}</h2>
        <p>{{ t('thirdPartyIntegration.tenant.description') }}</p>
      </div>
      <t-button variant="outline" :loading="loading" @click="loadAll">
        <template #icon><t-icon name="refresh" /></template>
        {{ t('common.refresh') }}
      </t-button>
    </div>

    <section class="settings-block">
      <div class="block-header">
        <div>
          <h3>{{ t('thirdPartyIntegration.connections.title') }}</h3>
          <p>{{ t('thirdPartyIntegration.connections.description') }}</p>
        </div>
        <span class="count-badge">{{ connections.length }}</span>
      </div>
      <div v-if="connections.length" class="connection-list">
        <article v-for="item in connections" :key="item.connection.id" class="connection-row">
          <div class="row-icon"><t-icon name="usercase-link" /></div>
          <div class="row-copy">
            <div class="row-title">
              <strong>{{ item.application.name }}</strong>
              <t-tag :theme="item.available ? 'success' : 'default'" variant="light" size="small">
                {{ item.available
                  ? t('thirdPartyIntegration.connections.connected')
                  : t('thirdPartyIntegration.connections.unavailable') }}
              </t-tag>
            </div>
            <p>
              {{ item.available ? t('thirdPartyIntegration.connections.summary', {
                count: item.knowledge_bases.length,
                scopes: item.effective_scopes.join(' · '),
              }) : t('thirdPartyIntegration.connections.unavailableSummary') }}
            </p>
            <div class="kb-chips">
              <t-tag v-for="kb in item.knowledge_bases.slice(0, 4)" :key="kb.id" size="small" variant="light">
                {{ kb.name }}
              </t-tag>
              <span v-if="item.knowledge_bases.length > 4">
                +{{ item.knowledge_bases.length - 4 }}
              </span>
            </div>
          </div>
          <div class="row-actions">
            <t-button
              size="small"
              variant="text"
              :disabled="!item.available"
              @click="openConnection(item.connection.id)"
            >
              {{ t('thirdPartyIntegration.connections.open') }}
            </t-button>
            <t-popconfirm
              theme="warning"
              :content="t('thirdPartyIntegration.connections.revokeConfirm', { name: item.application.name })"
              :confirm-btn="{ content: t('thirdPartyIntegration.connections.revoke'), theme: 'danger' }"
              :cancel-btn="t('common.cancel')"
              @confirm="revoke(item.connection.id)"
            >
              <t-button size="small" theme="danger" variant="text">
                {{ t('thirdPartyIntegration.connections.revoke') }}
              </t-button>
            </t-popconfirm>
          </div>
        </article>
      </div>
      <div v-else class="inline-empty">{{ t('thirdPartyIntegration.connections.empty') }}</div>
    </section>

    <section class="settings-block">
      <div class="block-header">
        <div>
          <h3>{{ t('thirdPartyIntegration.tenant.applications') }}</h3>
          <p>{{ t('thirdPartyIntegration.tenant.applicationsDescription') }}</p>
        </div>
      </div>
      <t-loading :loading="loading" show-overlay>
        <div v-if="applications.length" class="application-list">
          <article v-for="item in applications" :key="item.application.id" class="application-row">
            <div class="row-icon row-icon--app"><t-icon name="link" /></div>
            <div class="row-copy">
              <div class="row-title">
                <strong>{{ item.application.name }}</strong>
                <t-tag :theme="isAvailable(item) ? 'success' : 'default'" variant="light" size="small">
                  {{ isAvailable(item) ? t('common.on') : t('common.off') }}
                </t-tag>
              </div>
              <p>{{ item.application.description || t('thirdPartyIntegration.system.noDescription') }}</p>
              <span class="policy-summary">{{ policySummary(item) }}</span>
            </div>
            <t-button v-if="canManage" size="small" variant="outline" @click="openPolicy(item)">
              {{ t('thirdPartyIntegration.tenant.configure') }}
            </t-button>
          </article>
        </div>
        <div v-else-if="!loading" class="inline-empty">
          {{ t('thirdPartyIntegration.tenant.empty') }}
        </div>
      </t-loading>
    </section>

    <SettingDrawer
      v-model:visible="policyVisible"
      :title="t('thirdPartyIntegration.policy.title', { name: selectedApplication?.name || '' })"
      :description="t('thirdPartyIntegration.policy.description')"
      icon="lock-on"
      width="620px"
      :confirm-loading="saving"
      @confirm="savePolicy"
    >
      <div class="policy-form">
        <div class="switch-row">
          <div>
            <label>{{ t('thirdPartyIntegration.policy.enabled') }}</label>
            <p>{{ t('thirdPartyIntegration.policy.enabledHint') }}</p>
          </div>
          <t-switch v-model="policyForm.enabled" :disabled="!selectedApplication?.enabled" />
        </div>

        <label>{{ t('thirdPartyIntegration.fields.scopes') }}</label>
        <div class="scope-options">
          <t-checkbox v-model="policyRead" disabled>knowledge.read</t-checkbox>
          <t-checkbox
            v-model="policyChat"
            :disabled="!selectedApplication?.allowed_scopes.includes('knowledge.chat')"
          >
            knowledge.chat
          </t-checkbox>
        </div>

        <label>{{ t('thirdPartyIntegration.policy.knowledgeBases') }}</label>
        <t-select
          v-model="policyForm.knowledgeBaseIds"
          multiple
          filterable
          clearable
          :options="knowledgeBaseOptions"
          :placeholder="t('thirdPartyIntegration.policy.allKnowledgeBases')"
        />
        <p>{{ t('thirdPartyIntegration.policy.knowledgeBasesHint') }}</p>
      </div>
    </SettingDrawer>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { useRouter } from 'vue-router'
import { MessagePlugin } from 'tdesign-vue-next'
import { useI18n } from 'vue-i18n'
import { useAuthStore } from '@/stores/auth'
import SettingDrawer from '@/components/settings/SettingDrawer.vue'
import {
  listIntegrationConnections,
  listTenantIntegrationApplications,
  listTenantIntegrationKnowledgeBases,
  revokeIntegrationConnection,
  updateTenantIntegrationPolicy,
  type IntegrationApplication,
  type IntegrationConnectionView,
  type TenantIntegrationApplicationView,
} from '@/api/integration'

const { t } = useI18n()
const router = useRouter()
const authStore = useAuthStore()
const applications = ref<TenantIntegrationApplicationView[]>([])
const connections = ref<IntegrationConnectionView[]>([])
const knowledgeBases = ref<Array<{ id: string; name: string }>>([])
const loading = ref(false)
const saving = ref(false)
const policyVisible = ref(false)
const selectedApplication = ref<IntegrationApplication | null>(null)
const selectedPolicyItem = ref<TenantIntegrationApplicationView | null>(null)
const policyRead = ref(true)
const policyChat = ref(false)
const policyForm = reactive({ enabled: true, knowledgeBaseIds: [] as string[] })

const canManage = computed(() => authStore.canAccessAllTenants || authStore.hasRole('admin'))
const knowledgeBaseOptions = computed(() =>
  knowledgeBases.value.map(kb => ({ label: kb.name, value: kb.id })),
)

function isAvailable(item: TenantIntegrationApplicationView) {
  return item.application.enabled && (item.policy?.enabled ?? true)
}

function policySummary(item: TenantIntegrationApplicationView) {
  const scopes = item.policy?.allowed_scopes || item.application.allowed_scopes
  const count = item.policy?.knowledge_base_ids?.length || 0
  return count
    ? t('thirdPartyIntegration.tenant.restrictedSummary', { scopes: scopes.join(' · '), count })
    : t('thirdPartyIntegration.tenant.unrestrictedSummary', { scopes: scopes.join(' · ') })
}

async function loadAll() {
  loading.value = true
  try {
    const [applicationResponse, connectionResponse, kbResponse] = await Promise.all([
      listTenantIntegrationApplications(),
      listIntegrationConnections(),
      listTenantIntegrationKnowledgeBases(),
    ])
    applications.value = applicationResponse.data || []
    connections.value = connectionResponse.data || []
    knowledgeBases.value = kbResponse.data || []
  } catch (error: any) {
    MessagePlugin.error(error?.message || t('thirdPartyIntegration.tenant.loadFailed'))
  } finally {
    loading.value = false
  }
}

function openPolicy(item: TenantIntegrationApplicationView) {
  selectedPolicyItem.value = item
  selectedApplication.value = item.application
  policyForm.enabled = item.policy?.enabled ?? true
  policyForm.knowledgeBaseIds = [...(item.policy?.knowledge_base_ids || [])]
  const scopes = item.policy?.allowed_scopes || item.application.allowed_scopes
  policyRead.value = true
  policyChat.value = item.application.allowed_scopes.includes('knowledge.chat') && scopes.includes('knowledge.chat')
  policyVisible.value = true
}

async function savePolicy() {
  if (!selectedPolicyItem.value || !selectedApplication.value) return
  saving.value = true
  try {
    await updateTenantIntegrationPolicy(selectedApplication.value.id, {
      enabled: policyForm.enabled,
      allowed_scopes: policyChat.value
        ? ['knowledge.read', 'knowledge.chat']
        : ['knowledge.read'],
      knowledge_base_ids: policyForm.knowledgeBaseIds,
    })
    policyVisible.value = false
    MessagePlugin.success(t('thirdPartyIntegration.tenant.saved'))
    await loadAll()
  } catch (error: any) {
    MessagePlugin.error(error?.message || t('thirdPartyIntegration.tenant.saveFailed'))
  } finally {
    saving.value = false
  }
}

function openConnection(id: string) {
  router.push(`/integrations/launch/${encodeURIComponent(id)}`)
}

async function revoke(id: string) {
  try {
    await revokeIntegrationConnection(id)
    MessagePlugin.success(t('thirdPartyIntegration.connections.revoked'))
    await loadAll()
  } catch (error: any) {
    MessagePlugin.error(error?.message || t('thirdPartyIntegration.connections.revokeFailed'))
  }
}

onMounted(loadAll)
</script>

<style lang="less" scoped>
.integration-settings { display: grid; gap: 26px; }
.section-header { display: flex; align-items: flex-start; justify-content: space-between; gap: 20px; }
.section-header h2 { margin: 0 0 8px; font-size: 22px; }
.section-header p, .block-header p, .row-copy p, .policy-form p { margin: 0; color: var(--td-text-color-secondary); line-height: 1.55; }
.settings-block { overflow: hidden; border: 1px solid var(--td-component-stroke); border-radius: 10px; }
.block-header { display: flex; align-items: center; justify-content: space-between; gap: 12px; padding: 16px 18px; background: var(--td-bg-color-secondarycontainer); }
.block-header h3 { margin: 0 0 4px; font-size: 15px; }
.block-header p { font-size: 13px; }
.count-badge { min-width: 24px; padding: 2px 8px; border-radius: 999px; text-align: center; color: var(--td-brand-color); background: var(--td-brand-color-light); font-size: 12px; }
.connection-list, .application-list { display: grid; }
.connection-row, .application-row { display: flex; align-items: center; gap: 13px; padding: 16px 18px; border-top: 1px solid var(--td-component-stroke); }
.connection-row:first-child, .application-row:first-child { border-top: 0; }
.row-icon { width: 36px; height: 36px; display: grid; place-items: center; flex: none; border-radius: 9px; color: var(--td-success-color); background: var(--td-success-color-light); }
.row-icon--app { color: var(--td-brand-color); background: var(--td-brand-color-light); }
.row-copy { min-width: 0; flex: 1; }
.row-title { display: flex; align-items: center; gap: 9px; margin-bottom: 5px; }
.row-copy p { font-size: 13px; }
.kb-chips { display: flex; align-items: center; flex-wrap: wrap; gap: 5px; margin-top: 9px; color: var(--td-text-color-placeholder); font-size: 12px; }
.row-actions { display: flex; align-items: center; flex: none; }
.policy-summary { display: block; margin-top: 7px; color: var(--td-text-color-placeholder); font-size: 12px; }
.inline-empty { padding: 34px 18px; text-align: center; color: var(--td-text-color-placeholder); }
.policy-form { display: grid; gap: 11px; }
.policy-form > label { margin-top: 10px; font-weight: 600; }
.policy-form > p { margin-top: -4px; font-size: 12px; }
.switch-row { display: flex; align-items: center; justify-content: space-between; gap: 20px; }
.switch-row label { font-weight: 600; }
.switch-row p { margin-top: 4px; font-size: 12px; }
.scope-options { display: flex; gap: 22px; padding: 12px; border-radius: 8px; background: var(--td-bg-color-secondarycontainer); }
@media (max-width: 760px) { .connection-row, .application-row, .section-header { align-items: stretch; flex-direction: column; } .row-actions { justify-content: flex-end; } }
</style>
