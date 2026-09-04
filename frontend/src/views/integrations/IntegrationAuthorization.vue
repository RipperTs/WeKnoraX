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
          <div class="app-mark"><t-icon name="link" /></div>
          <div>
            <span class="eyebrow">{{ t('thirdPartyIntegration.authorization.eyebrow') }}</span>
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

        <section class="workspace-section">
          <div class="workspace-header">
            <div>
              <h2>{{ t('thirdPartyIntegration.authorization.selectWorkspaces') }}</h2>
              <p>{{ t('thirdPartyIntegration.authorization.selectWorkspacesHint') }}</p>
            </div>
            <t-button size="small" variant="text" @click="toggleAll">
              {{ allSelected ? t('common.clear') : t('common.selectAll') }}
            </t-button>
          </div>
          <t-checkbox-group v-model="selectedTenantIDs" class="workspace-list">
            <label v-for="tenant in authorization.tenants" :key="tenant.id" class="workspace-option">
              <t-checkbox :value="tenant.id" />
              <div>
                <strong>{{ tenant.name }}</strong>
                <p>{{ tenant.description || roleLabel(tenant.role) }}</p>
              </div>
            </label>
          </t-checkbox-group>
          <div v-if="authorization.tenants.length === 0" class="empty-workspace">
            {{ t('thirdPartyIntegration.authorization.noWorkspaces') }}
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
              :disabled="selectedTenantIDs.length === 0"
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

const { t } = useI18n()
const route = useRoute()
const loading = ref(true)
const submitting = ref(false)
const errorMessage = ref('')
const authorization = ref<IntegrationAuthorizationView | null>(null)
const selectedTenantIDs = ref<number[]>([])

const allSelected = computed(() => {
  const total = authorization.value?.tenants.length || 0
  return total > 0 && selectedTenantIDs.value.length === total
})

function parametersFromRoute(): IntegrationAuthorizationParameters {
  return {
    client_id: String(route.query.client_id || ''),
    redirect_uri: String(route.query.redirect_uri || ''),
    state: String(route.query.state || ''),
    scope: String(route.query.scope || ''),
    code_challenge: String(route.query.code_challenge || ''),
    code_challenge_method: String(route.query.code_challenge_method || ''),
    prompt: String(route.query.prompt || ''),
  }
}

function toggleAll() {
  if (!authorization.value) return
  selectedTenantIDs.value = allSelected.value
    ? []
    : authorization.value.tenants.map(tenant => tenant.id)
}

function roleLabel(role: string) {
  return t(`tenantMember.role.${role}`)
}

async function submit(approved: boolean, reuseExisting = false) {
  submitting.value = true
  try {
    const response = await authorizeIntegration(
      {
        parameters: parametersFromRoute(),
        approved,
        reuse_existing: reuseExisting,
        tenant_ids: selectedTenantIDs.value,
      },
    )
    window.location.replace(response.data.redirect_uri)
  } catch (error: any) {
    errorMessage.value = error?.message || t('thirdPartyIntegration.authorization.submitFailed')
  } finally {
    submitting.value = false
  }
}

async function loadAuthorization() {
  try {
    const response = await getIntegrationAuthorization(parametersFromRoute())
    authorization.value = response.data
    selectedTenantIDs.value = [...(response.data.selected_tenant_ids || [])]
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
.authorization-page { min-height: 100vh; display: grid; place-items: center; padding: clamp(32px, 6vh, 64px) clamp(24px, 5vw, 48px); box-sizing: border-box; background: radial-gradient(circle at 50% -10%, color-mix(in srgb, var(--td-brand-color) 18%, transparent), transparent 42%), linear-gradient(180deg, color-mix(in srgb, var(--td-bg-color-page) 82%, var(--td-bg-color-container)), var(--td-bg-color-page)); }
.authorization-card { width: min(680px, 100%); overflow: hidden; border: 1px solid color-mix(in srgb, var(--td-brand-color) 16%, var(--td-component-stroke)); border-radius: 18px; background: var(--td-bg-color-container); box-shadow: 0 24px 70px rgba(15, 35, 75, 0.13), 0 2px 8px rgba(15, 35, 75, 0.05); }
.authorization-header { display: flex; gap: 16px; padding: 28px 30px 22px; }
.app-mark, .state-icon { width: 48px; height: 48px; display: grid; place-items: center; flex: none; border-radius: 13px; color: white; background: linear-gradient(145deg, var(--td-brand-color), var(--td-brand-color-active)); box-shadow: 0 10px 24px color-mix(in srgb, var(--td-brand-color) 26%, transparent); font-size: 24px; }
.eyebrow { color: var(--td-brand-color); font-size: 12px; font-weight: 700; letter-spacing: .08em; text-transform: uppercase; }
.authorization-header h1 { margin: 5px 0 7px; font-size: 22px; line-height: 1.35; }
.authorization-header p, .permission-row p, .workspace-header p, .workspace-option p, .authorization-footer p, .state-panel p { margin: 0; color: var(--td-text-color-secondary); line-height: 1.55; }
.authorization-card > :deep(.t-alert) { margin: 0 30px; }
.permission-section, .workspace-section { margin: 24px 30px 0; }
.permission-section h2, .workspace-section h2 { margin: 0; font-size: 15px; }
.permission-list { display: grid; gap: 10px; margin-top: 12px; }
.permission-row { display: flex; align-items: flex-start; gap: 12px; padding: 13px 14px; border: 1px solid color-mix(in srgb, var(--td-brand-color) 10%, var(--td-component-stroke)); border-radius: 10px; background: color-mix(in srgb, var(--td-brand-color) 4%, var(--td-bg-color-container)); }
.permission-row > :first-child { margin-top: 2px; color: var(--td-brand-color); font-size: 18px; }
.permission-row strong, .workspace-option strong { font-size: 14px; }
.permission-row p, .workspace-option p { margin-top: 3px; font-size: 12px; }
.workspace-header { display: flex; align-items: flex-start; justify-content: space-between; gap: 16px; }
.workspace-header p { margin-top: 5px; font-size: 13px; }
.workspace-list { display: grid; gap: 10px; max-height: 280px; margin-top: 10px; padding: 8px 2px 12px; overflow-y: auto; }
.workspace-option { display: flex; align-items: flex-start; gap: 10px; padding: 16px 14px; border: 1px solid var(--td-component-stroke); border-radius: 10px; cursor: pointer; transition: border-color .2s, background .2s, transform .2s; }
.workspace-option:hover { border-color: var(--td-brand-color); background: var(--td-brand-color-light); transform: translateY(-1px); }
.workspace-option > :deep(.t-checkbox) { margin-top: 1px; }
.empty-workspace { margin-top: 14px; padding: 24px; text-align: center; border-radius: 9px; color: var(--td-text-color-placeholder); background: var(--td-bg-color-secondarycontainer); }
.authorization-footer { display: flex; align-items: center; justify-content: space-between; gap: 20px; margin-top: 26px; padding: 18px 30px; border-top: 1px solid var(--td-component-stroke); background: var(--td-bg-color-secondarycontainer); }
.authorization-footer > p { max-width: 330px; font-size: 12px; }
.authorization-footer > div { display: flex; gap: 10px; flex: none; }
.state-panel { display: grid; justify-items: center; gap: 12px; padding: 80px 30px; text-align: center; }
.state-panel h1 { margin: 6px 0 0; font-size: 21px; }
.state-panel--error .state-icon { background: var(--td-error-color); }
@media (max-width: 600px) { .authorization-header { padding: 22px 20px 18px; } .permission-section, .workspace-section { margin-inline: 20px; } .authorization-card > :deep(.t-alert) { margin-inline: 20px; } .authorization-footer { align-items: stretch; flex-direction: column; padding: 16px 20px; } .authorization-footer > div { justify-content: flex-end; } }
</style>
