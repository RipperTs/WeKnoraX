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
        <header>
          <div class="launch-icon"><t-icon name="link" /></div>
          <div>
            <span>{{ connection.application.name }}</span>
            <h1>{{ t('thirdPartyIntegration.launch.title') }}</h1>
            <p>{{ t('thirdPartyIntegration.launch.description') }}</p>
          </div>
        </header>
        <div class="launch-list">
          <button
            v-for="kb in connection.knowledge_bases"
            :key="kb.id"
            type="button"
            class="knowledge-link"
            @click="openKnowledgeBase(kb.id)"
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

function openKnowledgeBase(id: string) {
  router.push(`/platform/knowledge-bases/${encodeURIComponent(id)}`)
}

onMounted(async () => {
  try {
    const tenantId = Number(route.query.tenant_id)
    const targetTenantId = Number.isInteger(tenantId) && tenantId > 0 ? tenantId : undefined
    const response = await getIntegrationConnection(
      String(route.params.connectionId || ''),
      targetTenantId,
    )
    if (targetTenantId && targetTenantId !== Number(authStore.effectiveTenantId)) {
      authStore.setSelectedTenant(targetTenantId, null)
    }
    connection.value = response.data
    if (response.data.knowledge_bases.length === 1) {
      openKnowledgeBase(response.data.knowledge_bases[0].id)
    }
  } catch (error: any) {
    errorMessage.value = error?.message || t('thirdPartyIntegration.launch.loadFailed')
  } finally {
    loading.value = false
  }
})
</script>

<style lang="less" scoped>
.launch-page { min-height: 100vh; display: grid; place-items: center; padding: 24px 18px; box-sizing: border-box; background: var(--td-bg-color-page); }
.launch-card { width: min(660px, 100%); overflow: hidden; border: 1px solid var(--td-component-stroke); border-radius: 16px; background: var(--td-bg-color-container); box-shadow: 0 18px 55px rgba(15, 35, 75, .1); }
.launch-card header { display: flex; align-items: center; gap: 16px; padding: 26px; border-bottom: 1px solid var(--td-component-stroke); }
.launch-icon, .knowledge-icon { display: grid; place-items: center; flex: none; color: var(--td-brand-color); background: var(--td-brand-color-light); }
.launch-icon { width: 46px; height: 46px; border-radius: 13px; font-size: 22px; }
.launch-card header span { color: var(--td-brand-color); font-size: 12px; font-weight: 600; }
.launch-card h1 { margin: 4px 0 5px; font-size: 21px; }
.launch-card p { margin: 0; color: var(--td-text-color-secondary); }
.launch-list { display: grid; gap: 9px; padding: 18px; }
.knowledge-link { width: 100%; display: flex; align-items: center; gap: 12px; padding: 13px; text-align: left; border: 1px solid var(--td-component-stroke); border-radius: 10px; color: var(--td-text-color-primary); background: transparent; cursor: pointer; transition: border-color .2s, background .2s; }
.knowledge-link:hover { border-color: var(--td-brand-color); background: var(--td-brand-color-light); }
.knowledge-icon { width: 34px; height: 34px; border-radius: 8px; }
.knowledge-copy { min-width: 0; flex: 1; display: grid; gap: 4px; }
.knowledge-copy small { overflow: hidden; color: var(--td-text-color-secondary); text-overflow: ellipsis; white-space: nowrap; }
.launch-empty, .launch-state { padding: 64px 24px; text-align: center; color: var(--td-text-color-secondary); }
.launch-state { display: grid; justify-items: center; gap: 12px; }
.launch-state h1 { margin: 0; }
.launch-state--error > :first-child { color: var(--td-error-color); }
</style>
