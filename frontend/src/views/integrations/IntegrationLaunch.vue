<template>
  <main class="launch-page">
    <section class="launch-card">
      <div v-if="loading" class="launch-state">
        <t-loading size="large" />
        <p>{{ t('thirdPartyIntegration.launch.loading') }}</p>
      </div>
      <div v-else-if="errorMessage" class="launch-state launch-state--error">
        <t-icon name="error-circle" size="40px" />
        <h1>{{ t('thirdPartyIntegration.launch.errorTitle') }}</h1>
        <p>{{ errorMessage }}</p>
      </div>
      <template v-else-if="connection">
        <header class="launch-hero">
          <div class="success-mark"><t-icon name="check-circle-filled" /></div>
          <div class="launch-copy">
            <span class="success-label">{{ t('thirdPartyIntegration.launch.successLabel') }}</span>
            <h1>{{ t('thirdPartyIntegration.launch.title') }}</h1>
            <p>{{ t('thirdPartyIntegration.launch.description', { name: connection.application.name }) }}</p>
          </div>
        </header>

        <section class="connection-overview">
          <div class="overview-item">
            <span>{{ connection.tenants.length }}</span>
            <small>{{ t('thirdPartyIntegration.launch.workspaceCount') }}</small>
          </div>
          <div class="overview-divider" />
          <div class="overview-item">
            <span>{{ connection.knowledge_bases.length }}</span>
            <small>{{ t('thirdPartyIntegration.launch.knowledgeBaseCount') }}</small>
          </div>
        </section>

        <section class="workspace-scope">
          <div class="section-title">
            <t-icon name="layers" />
            <strong>{{ t('thirdPartyIntegration.launch.workspaces') }}</strong>
          </div>
          <div class="workspace-tags">
            <t-tag v-for="tenant in connection.tenants" :key="tenant.id" size="small" variant="light">
              {{ tenant.name }}
            </t-tag>
          </div>
        </section>

        <div class="knowledge-heading">
          <div class="section-title">
            <t-icon name="folder" />
            <strong>{{ t('thirdPartyIntegration.launch.knowledgeBases') }}</strong>
          </div>
          <span>{{ connection.knowledge_bases.length }}</span>
        </div>
        <div class="launch-list">
          <button
            v-for="kb in connection.knowledge_bases"
            :key="kb.id"
            type="button"
            class="knowledge-link"
            @click="openKnowledgeBase(kb.id, kb.access_tenant_id)"
          >
            <span class="knowledge-icon"><t-icon name="folder" /></span>
            <span class="knowledge-copy">
              <strong>{{ kb.name }}</strong>
              <small>{{ kb.description || t('thirdPartyIntegration.launch.openHint') }}</small>
            </span>
            <t-icon name="chevron-right" />
          </button>
        </div>
        <div v-if="connection.knowledge_bases.length === 0" class="launch-empty">
          {{ t('thirdPartyIntegration.launch.empty') }}
        </div>
      </template>
    </section>
  </main>
</template>

<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { useI18n } from 'vue-i18n'
import { getIntegrationConnection, type IntegrationConnectionView } from '@/api/integration'
import { useAuthStore } from '@/stores/auth'

const { t } = useI18n()
const route = useRoute()
const router = useRouter()
const authStore = useAuthStore()
const loading = ref(true)
const errorMessage = ref('')
const connection = ref<IntegrationConnectionView | null>(null)

function openKnowledgeBase(id: string, tenantId?: number) {
  const tenant = connection.value?.tenants.find(item => item.id === tenantId)
  if (tenantId && tenantId !== Number(authStore.effectiveTenantId)) {
    authStore.setSelectedTenant(tenantId, tenant?.name || null)
  }
  router.push(`/platform/knowledge-bases/${encodeURIComponent(id)}`)
}

onMounted(async () => {
  try {
    const response = await getIntegrationConnection(String(route.params.connectionId || ''))
    connection.value = response.data
    if (response.data.knowledge_bases.length === 1) {
      const knowledgeBase = response.data.knowledge_bases[0]
      openKnowledgeBase(knowledgeBase.id, knowledgeBase.access_tenant_id)
    }
  } catch (error: any) {
    errorMessage.value = error?.message || t('thirdPartyIntegration.launch.loadFailed')
  } finally {
    loading.value = false
  }
})
</script>

<style lang="less" scoped>
.launch-page { min-height: 100vh; display: grid; place-items: center; padding: clamp(32px, 6vh, 64px) clamp(24px, 5vw, 48px); box-sizing: border-box; background: radial-gradient(circle at 50% -12%, color-mix(in srgb, var(--td-success-color) 17%, transparent), transparent 40%), linear-gradient(180deg, color-mix(in srgb, var(--td-bg-color-page) 82%, var(--td-bg-color-container)), var(--td-bg-color-page)); }
.launch-card { width: min(680px, 100%); overflow: hidden; border: 1px solid color-mix(in srgb, var(--td-success-color) 15%, var(--td-component-stroke)); border-radius: 18px; background: var(--td-bg-color-container); box-shadow: 0 24px 70px rgba(15, 52, 44, .12), 0 2px 8px rgba(15, 35, 75, .05); }
.launch-hero { display: grid; justify-items: center; padding: 34px 30px 24px; text-align: center; }
.success-mark { width: 58px; height: 58px; display: grid; place-items: center; margin-bottom: 14px; border-radius: 18px; color: var(--td-success-color); background: var(--td-success-color-light); box-shadow: 0 12px 28px color-mix(in srgb, var(--td-success-color) 18%, transparent); font-size: 30px; }
.launch-copy { max-width: 500px; }
.success-label { color: var(--td-success-color); font-size: 12px; font-weight: 700; letter-spacing: .08em; text-transform: uppercase; }
.launch-card h1 { margin: 6px 0 8px; font-size: 24px; line-height: 1.35; }
.launch-card p { margin: 0; color: var(--td-text-color-secondary); line-height: 1.6; }
.connection-overview { display: grid; grid-template-columns: 1fr auto 1fr; align-items: center; margin: 0 30px 24px; padding: 15px 20px; border-radius: 12px; background: color-mix(in srgb, var(--td-success-color) 6%, var(--td-bg-color-container)); }
.overview-item { display: grid; justify-items: center; gap: 2px; }
.overview-item span { color: var(--td-text-color-primary); font-size: 22px; font-weight: 700; }
.overview-item small { color: var(--td-text-color-secondary); font-size: 12px; }
.overview-divider { width: 1px; height: 30px; background: var(--td-component-stroke); }
.workspace-scope { display: flex; align-items: center; justify-content: space-between; gap: 16px; padding: 14px 20px; border-block: 1px solid var(--td-component-stroke); background: var(--td-bg-color-secondarycontainer); font-size: 13px; }
.section-title { display: flex; align-items: center; gap: 8px; }
.section-title > :first-child { color: var(--td-brand-color); font-size: 17px; }
.workspace-tags { display: flex; justify-content: flex-end; flex-wrap: wrap; gap: 6px; }
.knowledge-heading { display: flex; align-items: center; justify-content: space-between; padding: 18px 20px 0; font-size: 14px; }
.knowledge-heading > span { min-width: 24px; height: 24px; display: grid; place-items: center; border-radius: 12px; color: var(--td-text-color-secondary); background: var(--td-bg-color-secondarycontainer); font-size: 12px; }
.launch-list { display: grid; gap: 9px; padding: 12px 20px 20px; }
.knowledge-link { width: 100%; display: flex; align-items: center; gap: 12px; padding: 13px 14px; text-align: left; border: 1px solid var(--td-component-stroke); border-radius: 10px; color: var(--td-text-color-primary); background: transparent; cursor: pointer; transition: border-color .2s, background .2s, transform .2s; }
.knowledge-link:hover { border-color: var(--td-brand-color); background: var(--td-brand-color-light); transform: translateY(-1px); }
.knowledge-link:focus-visible { outline: 2px solid var(--td-brand-color); outline-offset: 2px; }
.knowledge-icon { width: 36px; height: 36px; display: grid; place-items: center; flex: none; border-radius: 9px; color: var(--td-brand-color); background: var(--td-brand-color-light); }
.knowledge-copy { min-width: 0; flex: 1; display: grid; gap: 4px; }
.knowledge-copy small { overflow: hidden; color: var(--td-text-color-secondary); text-overflow: ellipsis; white-space: nowrap; }
.launch-empty, .launch-state { padding: 64px 24px; text-align: center; color: var(--td-text-color-secondary); }
.launch-state { display: grid; justify-items: center; gap: 12px; }
.launch-state h1 { margin: 0; }
.launch-state--error > :first-child { color: var(--td-error-color); }
@media (max-width: 600px) { .launch-hero { padding: 28px 20px 22px; } .connection-overview { margin-inline: 20px; } .workspace-scope { align-items: flex-start; flex-direction: column; } .workspace-tags { justify-content: flex-start; } }
@media (prefers-reduced-motion: reduce) { .knowledge-link { transition: none; } }
</style>
