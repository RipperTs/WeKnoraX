<template>
  <div class="third-party-apps">
    <div class="section-header">
      <div>
        <h2>{{ t('thirdPartyIntegration.system.title') }}</h2>
        <p>{{ t('thirdPartyIntegration.system.description') }}</p>
      </div>
      <t-button
        type="button"
        theme="primary"
        variant="text"
        size="medium"
        class="section-action-trigger"
        @click="openCreate"
      >
        <template #icon><t-icon name="add" /></template>
        {{ t('thirdPartyIntegration.system.create') }}
      </t-button>
    </div>

    <t-loading :loading="loading" show-overlay>
      <div v-if="apps.length" class="app-list">
        <article v-for="app in apps" :key="app.id" class="app-card">
          <div class="app-card__main">
            <div class="app-icon"><t-icon name="link" /></div>
            <div class="app-copy">
              <div class="app-title-row">
                <h3>{{ app.name }}</h3>
                <t-tag :theme="app.enabled ? 'success' : 'default'" variant="light" size="small">
                  {{ app.enabled ? t('common.on') : t('common.off') }}
                </t-tag>
              </div>
              <p>{{ app.description || t('thirdPartyIntegration.system.noDescription') }}</p>
              <div class="app-meta">
                <code>{{ app.client_id }}</code>
                <span>{{ scopeLabels(app.allowed_scopes) }}</span>
                <span>{{ t('thirdPartyIntegration.system.redirectCount', { count: app.redirect_uris.length }) }}</span>
              </div>
            </div>
          </div>
          <div class="app-actions">
            <t-button size="small" variant="outline" @click="openEdit(app)">
              {{ t('common.edit') }}
            </t-button>
            <t-button size="small" variant="text" @click="confirmRotate(app)">
              {{ t('thirdPartyIntegration.system.rotateSecret') }}
            </t-button>
          </div>
        </article>
      </div>
      <div v-else-if="!loading" class="empty-state">
        <t-icon name="link-unlink" size="42px" />
        <h3>{{ t('thirdPartyIntegration.system.emptyTitle') }}</h3>
        <p>{{ t('thirdPartyIntegration.system.emptyDescription') }}</p>
      </div>
    </t-loading>

    <SettingDrawer
      v-model:visible="drawerVisible"
      :title="editingId ? t('thirdPartyIntegration.system.edit') : t('thirdPartyIntegration.system.create')"
      :description="t('thirdPartyIntegration.system.formDescription')"
      icon="link"
      width="620px"
      :confirm-loading="saving"
      @confirm="saveApplication"
    >
      <div class="form-stack">
        <label>{{ t('thirdPartyIntegration.fields.name') }}</label>
        <t-input v-model="form.name" :maxlength="128" />

        <label>{{ t('thirdPartyIntegration.fields.description') }}</label>
        <t-textarea v-model="form.description" :maxlength="2000" :autosize="{ minRows: 3, maxRows: 6 }" />

        <label>{{ t('thirdPartyIntegration.fields.redirectUris') }}</label>
        <t-textarea
          v-model="redirectURIText"
          :placeholder="t('thirdPartyIntegration.fields.redirectUrisPlaceholder')"
          :autosize="{ minRows: 4, maxRows: 8 }"
        />
        <p class="field-hint">{{ t('thirdPartyIntegration.fields.redirectUrisHint') }}</p>

        <label>{{ t('thirdPartyIntegration.fields.scopes') }}</label>
        <div class="scope-options">
          <t-checkbox v-model="readScope" disabled>knowledge.read</t-checkbox>
          <t-checkbox v-model="chatScope">knowledge.chat</t-checkbox>
        </div>

        <div class="switch-row">
          <div>
            <label>{{ t('thirdPartyIntegration.fields.enabled') }}</label>
            <p class="field-hint">{{ t('thirdPartyIntegration.fields.enabledHint') }}</p>
          </div>
          <t-switch v-model="form.enabled" />
        </div>
      </div>
    </SettingDrawer>

    <t-dialog
      v-model:visible="secretVisible"
      :header="t('thirdPartyIntegration.secret.title')"
      :confirm-btn="{ content: t('thirdPartyIntegration.secret.copy'), theme: 'primary' }"
      :cancel-btn="null"
      :close-on-overlay-click="false"
      @confirm="copySecret"
    >
      <t-alert theme="warning" :message="t('thirdPartyIntegration.secret.warning')" />
      <div class="secret-field">
        <span>Client ID</span>
        <code>{{ secretClientID }}</code>
      </div>
      <div class="secret-field">
        <span>Client Secret</span>
        <t-textarea :value="createdSecret" readonly autosize />
      </div>
    </t-dialog>
  </div>
</template>

<script setup lang="ts">
import { onMounted, reactive, ref } from 'vue'
import { DialogPlugin, MessagePlugin } from 'tdesign-vue-next'
import { useI18n } from 'vue-i18n'
import SettingDrawer from '@/components/settings/SettingDrawer.vue'
import { copyWithToast } from '@/utils/clipboard'
import {
  createIntegrationApplication,
  listIntegrationApplications,
  rotateIntegrationApplicationSecret,
  updateIntegrationApplication,
  type IntegrationApplication,
  type IntegrationApplicationInput,
  type IntegrationScope,
} from '@/api/integration'

const { t } = useI18n()
const apps = ref<IntegrationApplication[]>([])
const loading = ref(false)
const saving = ref(false)
const drawerVisible = ref(false)
const editingId = ref('')
const redirectURIText = ref('')
const readScope = ref(true)
const chatScope = ref(false)
const secretVisible = ref(false)
const createdSecret = ref('')
const secretClientID = ref('')
const form = reactive({ name: '', description: '', enabled: true })

function scopeLabels(scopes: IntegrationScope[]) {
  return scopes.join(' · ')
}

function resetForm() {
  editingId.value = ''
  form.name = ''
  form.description = ''
  form.enabled = true
  redirectURIText.value = ''
  readScope.value = true
  chatScope.value = false
}

function openCreate() {
  resetForm()
  drawerVisible.value = true
}

function openEdit(app: IntegrationApplication) {
  editingId.value = app.id
  form.name = app.name
  form.description = app.description
  form.enabled = app.enabled
  redirectURIText.value = app.redirect_uris.join('\n')
  readScope.value = true
  chatScope.value = app.allowed_scopes.includes('knowledge.chat')
  drawerVisible.value = true
}

function applicationInput(): IntegrationApplicationInput | null {
  const redirectUris = redirectURIText.value
    .split(/\r?\n/)
    .map(value => value.trim())
    .filter(Boolean)
  if (!form.name.trim() || redirectUris.length === 0) {
    MessagePlugin.warning(t('thirdPartyIntegration.system.required'))
    return null
  }
  return {
    name: form.name.trim(),
    description: form.description.trim(),
    redirect_uris: [...new Set(redirectUris)],
    allowed_scopes: chatScope.value
      ? ['knowledge.read', 'knowledge.chat']
      : ['knowledge.read'],
    enabled: form.enabled,
  }
}

async function loadApplications() {
  loading.value = true
  try {
    const response = await listIntegrationApplications()
    apps.value = response.data || []
  } catch (error: any) {
    MessagePlugin.error(error?.message || t('thirdPartyIntegration.system.loadFailed'))
  } finally {
    loading.value = false
  }
}

async function saveApplication() {
  const input = applicationInput()
  if (!input) return
  saving.value = true
  try {
    if (editingId.value) {
      await updateIntegrationApplication(editingId.value, input)
      MessagePlugin.success(t('thirdPartyIntegration.system.saved'))
    } else {
      const response = await createIntegrationApplication(input)
      createdSecret.value = response.data.client_secret
      secretClientID.value = response.data.application.client_id
      secretVisible.value = true
    }
    drawerVisible.value = false
    await loadApplications()
  } catch (error: any) {
    MessagePlugin.error(error?.message || t('thirdPartyIntegration.system.saveFailed'))
  } finally {
    saving.value = false
  }
}

function confirmRotate(app: IntegrationApplication) {
  const dialog = DialogPlugin.confirm({
    header: t('thirdPartyIntegration.system.rotateTitle'),
    body: t('thirdPartyIntegration.system.rotateConfirm', { name: app.name }),
    onConfirm: async () => {
      dialog.destroy()
      try {
        const response = await rotateIntegrationApplicationSecret(app.id)
        createdSecret.value = response.data.client_secret
        secretClientID.value = response.data.application.client_id
        secretVisible.value = true
      } catch (error: any) {
        MessagePlugin.error(error?.message || t('thirdPartyIntegration.system.rotateFailed'))
      }
    },
    onCancel: () => dialog.destroy(),
  })
}

async function copySecret() {
  await copyWithToast(createdSecret.value, 'common.copySuccess')
}

onMounted(loadApplications)
</script>

<style lang="less" scoped>
.third-party-apps { min-height: 420px; }
.section-header { display: flex; align-items: center; justify-content: space-between; gap: 24px; margin-bottom: 24px; }
.section-header h2 { margin: 0 0 8px; font-size: 22px; }
.section-header p { margin: 0; color: var(--td-text-color-secondary); line-height: 1.6; }
.section-action-trigger {
  --td-bg-color-container-hover: transparent;
  flex-shrink: 0;
  padding-left: 0;
  padding-right: 0;
  font-weight: 600;
}
.section-action-trigger:hover,
.section-action-trigger:focus,
.section-action-trigger.t-is-active,
.section-action-trigger:active {
  background-color: transparent !important;
  color: var(--td-brand-color-hover);
}
.section-action-trigger:active { color: var(--td-brand-color-active); }
.app-list { display: grid; gap: 12px; }
.app-card { display: flex; align-items: center; justify-content: space-between; gap: 18px; padding: 18px; border: 1px solid var(--td-component-stroke); border-radius: 10px; background: var(--td-bg-color-container); }
.app-card__main { display: flex; min-width: 0; gap: 14px; }
.app-icon { width: 40px; height: 40px; display: grid; place-items: center; flex: none; border-radius: 10px; color: var(--td-brand-color); background: var(--td-brand-color-light); font-size: 20px; }
.app-copy { min-width: 0; }
.app-title-row { display: flex; align-items: center; gap: 10px; }
.app-title-row h3 { margin: 0; font-size: 16px; }
.app-copy > p { margin: 7px 0 10px; color: var(--td-text-color-secondary); }
.app-meta { display: flex; flex-wrap: wrap; gap: 8px 14px; color: var(--td-text-color-placeholder); font-size: 12px; }
.app-meta code { color: var(--td-text-color-secondary); }
.app-actions { display: flex; align-items: center; flex: none; }
.empty-state { padding: 72px 20px; text-align: center; color: var(--td-text-color-placeholder); }
.empty-state h3 { margin: 14px 0 6px; color: var(--td-text-color-primary); }
.empty-state p { margin: 0; }
.form-stack { display: grid; gap: 10px; }
.form-stack > label, .switch-row label { margin-top: 8px; font-weight: 600; color: var(--td-text-color-primary); }
.field-hint { margin: -2px 0 4px; color: var(--td-text-color-placeholder); font-size: 12px; line-height: 1.5; }
.scope-options { display: flex; gap: 22px; padding: 12px; border-radius: 8px; background: var(--td-bg-color-secondarycontainer); }
.switch-row { display: flex; align-items: center; justify-content: space-between; gap: 20px; margin-top: 8px; }
.switch-row .field-hint { margin: 4px 0 0; }
.secret-field { display: grid; gap: 6px; margin-top: 16px; }
.secret-field span { color: var(--td-text-color-secondary); font-size: 13px; }
.secret-field code { padding: 10px 12px; overflow-wrap: anywhere; border-radius: 6px; background: var(--td-bg-color-secondarycontainer); }
@media (max-width: 760px) { .app-card, .section-header { align-items: stretch; flex-direction: column; } .app-actions { justify-content: flex-end; } }
</style>
