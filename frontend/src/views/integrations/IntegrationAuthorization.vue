<template>
  <main class="authorization-page">
    <section class="authorization-card">
      <div v-if="loading" class="state-panel">
        <t-loading size="large" />
        <p>{{ t('thirdPartyIntegration.authorization.loading') }}</p>
      </div>

      <div v-else-if="errorMessage" class="state-panel state-panel--error">
        <div class="state-icon"><t-icon name="error-circle" /></div>
        <h1>{{ t('thirdPartyIntegration.authorization.errorTitle') }}</h1>
        <p>{{ errorMessage }}</p>
      </div>

      <template v-else-if="authorization">
        <header class="authorization-header">
          <div class="brand-mark"><t-icon name="link" /></div>
          <div>
            <span class="eyebrow">WeKnora</span>
            <h1>{{ t('thirdPartyIntegration.authorization.title', { name: authorization.application.name }) }}</h1>
            <p>{{ authorization.application.description || t('thirdPartyIntegration.system.noDescription') }}</p>
          </div>
        </header>

        <t-alert theme="info" :message="t('thirdPartyIntegration.authorization.credentialNotice')" />

        <section class="permission-section">
          <h2>{{ t('thirdPartyIntegration.authorization.permissions') }}</h2>
          <div class="permission-list">
            <div class="permission-row">
              <t-icon name="search" />
              <div>
                <strong>{{ t('thirdPartyIntegration.authorization.readTitle') }}</strong>
                <p>{{ t('thirdPartyIntegration.authorization.readDescription') }}</p>
              </div>
            </div>
            <div v-if="authorization.scopes.includes('knowledge.chat')" class="permission-row">
              <t-icon name="chat" />
              <div>
                <strong>{{ t('thirdPartyIntegration.authorization.chatTitle') }}</strong>
                <p>{{ t('thirdPartyIntegration.authorization.chatDescription') }}</p>
              </div>
            </div>
          </div>
        </section>

        <section class="knowledge-section">
          <div class="knowledge-header">
            <div>
              <h2>{{ t('thirdPartyIntegration.authorization.selectKnowledgeBases') }}</h2>
              <p>{{ t('thirdPartyIntegration.authorization.selectKnowledgeBasesHint') }}</p>
            </div>
            <t-button size="small" variant="text" @click="toggleAll">
              {{ allSelected ? t('common.clear') : t('common.selectAll') }}
            </t-button>
          </div>
          <t-checkbox-group v-model="selectedIDs" class="knowledge-list">
            <label v-for="kb in authorization.knowledge_bases" :key="kb.id" class="knowledge-option">
              <t-checkbox :value="kb.id" />
              <div>
                <strong>{{ kb.name }}</strong>
                <p>{{ kb.description || knowledgeBaseTypeLabel(kb.type) }}</p>
              </div>
            </label>
          </t-checkbox-group>
          <div v-if="authorization.knowledge_bases.length === 0" class="empty-knowledge">
            {{ t('thirdPartyIntegration.authorization.noKnowledgeBases') }}
          </div>
        </section>

        <footer class="authorization-footer">
          <p>{{ t('thirdPartyIntegration.authorization.footerHint') }}</p>
          <div>
            <t-button variant="outline" :disabled="submitting" @click="submit(false)">
              {{ t('thirdPartyIntegration.authorization.deny') }}
            </t-button>
            <t-button
              theme="primary"
              :loading="submitting"
              :disabled="selectedIDs.length === 0"
              @click="submit(true)"
            >
              {{ t('thirdPartyIntegration.authorization.allow') }}
            </t-button>
          </div>
        </footer>
      </template>
    </section>
  </main>
</template>

<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useRoute } from 'vue-router'
import { useI18n } from 'vue-i18n'
import {
  authorizeIntegration,
  getIntegrationAuthorization,
  type IntegrationAuthorizationParameters,
  type IntegrationAuthorizationView,
} from '@/api/integration'
import { useAuthStore } from '@/stores/auth'

const { t } = useI18n()
const route = useRoute()
const authStore = useAuthStore()
const loading = ref(true)
const submitting = ref(false)
const errorMessage = ref('')
const authorization = ref<IntegrationAuthorizationView | null>(null)
const selectedIDs = ref<string[]>([])

const allSelected = computed(() => {
  const total = authorization.value?.knowledge_bases.length || 0
  return total > 0 && selectedIDs.value.length === total
})

function parametersFromRoute(): IntegrationAuthorizationParameters {
  return {
    client_id: String(route.query.client_id || ''),
    redirect_uri: String(route.query.redirect_uri || ''),
    state: String(route.query.state || ''),
    scope: String(route.query.scope || ''),
    code_challenge: String(route.query.code_challenge || ''),
    code_challenge_method: String(route.query.code_challenge_method || ''),
  }
}

function toggleAll() {
  if (!authorization.value) return
  selectedIDs.value = allSelected.value
    ? []
    : authorization.value.knowledge_bases.map(kb => kb.id)
}

function knowledgeBaseTypeLabel(type: string) {
  return t(`thirdPartyIntegration.authorization.knowledgeBaseType.${type}`)
}

async function submit(approved: boolean, reuseExisting = false) {
  submitting.value = true
  try {
    const response = await authorizeIntegration({
      parameters: parametersFromRoute(),
      approved,
      reuse_existing: reuseExisting,
      knowledge_base_ids: selectedIDs.value,
    })
    window.location.replace(response.data.redirect_uri)
  } catch (error: any) {
    errorMessage.value = error?.message || t('thirdPartyIntegration.authorization.submitFailed')
  } finally {
    submitting.value = false
  }
}

async function loadAuthorization() {
  try {
    const tenantId = Number(route.query.tenant_id)
    if (
      Number.isInteger(tenantId) &&
      tenantId > 0 &&
      tenantId !== Number(authStore.effectiveTenantId)
    ) {
      authStore.setSelectedTenant(tenantId, null)
    }
    const response = await getIntegrationAuthorization(parametersFromRoute())
    authorization.value = response.data
    selectedIDs.value = [...(response.data.selected_knowledge_base_ids || [])]
    if (!response.data.requires_consent) {
      await submit(true, true)
    }
  } catch (error: any) {
    errorMessage.value = error?.message || t('thirdPartyIntegration.authorization.loadFailed')
  } finally {
    loading.value = false
  }
}

onMounted(loadAuthorization)
</script>

<style lang="less" scoped>
.authorization-page { min-height: 100vh; display: grid; place-items: center; padding: 28px 18px; box-sizing: border-box; background: radial-gradient(circle at top, color-mix(in srgb, var(--td-brand-color) 12%, transparent), transparent 42%), var(--td-bg-color-page); }
.authorization-card { width: min(680px, 100%); overflow: hidden; border: 1px solid var(--td-component-stroke); border-radius: 16px; background: var(--td-bg-color-container); box-shadow: 0 18px 55px rgba(15, 35, 75, 0.12); }
.authorization-header { display: flex; gap: 16px; padding: 28px 30px 22px; }
.brand-mark, .state-icon { width: 48px; height: 48px; display: grid; place-items: center; flex: none; border-radius: 13px; color: white; background: linear-gradient(145deg, var(--td-brand-color), var(--td-brand-color-active)); font-size: 24px; }
.eyebrow { color: var(--td-brand-color); font-size: 12px; font-weight: 700; letter-spacing: .08em; text-transform: uppercase; }
.authorization-header h1 { margin: 5px 0 7px; font-size: 22px; line-height: 1.35; }
.authorization-header p, .permission-row p, .knowledge-header p, .knowledge-option p, .authorization-footer p, .state-panel p { margin: 0; color: var(--td-text-color-secondary); line-height: 1.55; }
.authorization-card > :deep(.t-alert) { margin: 0 30px; }
.permission-section, .knowledge-section { margin: 24px 30px 0; }
.permission-section h2, .knowledge-section h2 { margin: 0; font-size: 15px; }
.permission-list { display: grid; gap: 10px; margin-top: 12px; }
.permission-row { display: flex; align-items: flex-start; gap: 12px; padding: 12px; border-radius: 9px; background: var(--td-bg-color-secondarycontainer); }
.permission-row > :first-child { margin-top: 2px; color: var(--td-brand-color); font-size: 18px; }
.permission-row strong, .knowledge-option strong { font-size: 14px; }
.permission-row p, .knowledge-option p { margin-top: 3px; font-size: 12px; }
.knowledge-header { display: flex; align-items: flex-start; justify-content: space-between; gap: 16px; }
.knowledge-header p { margin-top: 5px; font-size: 13px; }
.knowledge-list { display: grid; gap: 8px; max-height: 280px; margin-top: 14px; overflow-y: auto; }
.knowledge-option { display: flex; align-items: flex-start; gap: 10px; padding: 12px; border: 1px solid var(--td-component-stroke); border-radius: 9px; cursor: pointer; transition: border-color .2s, background .2s; }
.knowledge-option:hover { border-color: var(--td-brand-color); background: var(--td-brand-color-light); }
.knowledge-option > :deep(.t-checkbox) { margin-top: 1px; }
.empty-knowledge { margin-top: 14px; padding: 24px; text-align: center; border-radius: 9px; color: var(--td-text-color-placeholder); background: var(--td-bg-color-secondarycontainer); }
.authorization-footer { display: flex; align-items: center; justify-content: space-between; gap: 20px; margin-top: 26px; padding: 18px 30px; border-top: 1px solid var(--td-component-stroke); background: var(--td-bg-color-secondarycontainer); }
.authorization-footer > p { max-width: 330px; font-size: 12px; }
.authorization-footer > div { display: flex; gap: 10px; flex: none; }
.state-panel { display: grid; justify-items: center; gap: 12px; padding: 80px 30px; text-align: center; }
.state-panel h1 { margin: 6px 0 0; font-size: 21px; }
.state-panel--error .state-icon { background: var(--td-error-color); }
@media (max-width: 600px) { .authorization-header { padding: 22px 20px 18px; } .permission-section, .knowledge-section { margin-inline: 20px; } .authorization-card > :deep(.t-alert) { margin-inline: 20px; } .authorization-footer { align-items: stretch; flex-direction: column; padding: 16px 20px; } .authorization-footer > div { justify-content: flex-end; } }
</style>
