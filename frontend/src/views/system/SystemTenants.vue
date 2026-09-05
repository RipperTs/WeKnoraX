<template>
  <div class="system-tenants">
    <div class="section-header">
      <h2>{{ t('systemTenants.title') }}</h2>
      <p>{{ t('systemTenants.description') }}</p>
    </div>

    <div class="tenants-toolbar">
      <div class="tenants-count">
        <strong>{{ total }}</strong>
        <span>{{ t('systemTenants.total') }}</span>
      </div>
      <div class="tenants-search">
        <t-input
          v-model="searchInput"
          clearable
          :placeholder="t('systemTenants.searchPlaceholder')"
          @enter="searchTenants"
          @clear="searchTenants"
        >
          <template #prefix-icon><t-icon name="search" /></template>
        </t-input>
        <t-button :loading="loading" @click="searchTenants">{{ t('systemTenants.search') }}</t-button>
      </div>
    </div>

    <t-alert v-if="error" theme="error" :message="error" />
    <div v-else class="tenants-table-shell">
      <div class="tenants-table-scroll">
        <t-table row-key="id" :data="tenants" :columns="columns" :loading="loading" hover>
          <template #workspace="{ row }">
            <div class="identity">
              <span class="workspace-name">{{ row.name }}</span>
              <span class="secondary">ID: {{ row.id }}</span>
            </div>
          </template>
          <template #owners="{ row }">
            <div v-for="owner in row.owners" :key="owner.user_id" class="identity owner">
              <span>{{ owner.username }}</span>
              <span class="secondary">{{ owner.email }}</span>
            </div>
            <span v-if="row.owners.length === 0" class="secondary">{{ t('systemTenants.noOwner') }}</span>
          </template>
          <template #storage_used="{ row }">{{ formatCapacity(row.storage_used) }}</template>
          <template #storage_quota="{ row }">
            {{ row.storage_quota > 0 ? formatCapacity(row.storage_quota) : t('systemTenants.unlimited') }}
          </template>
          <template #actions="{ row }">
            <t-button
              variant="text"
              theme="primary"
              :disabled="row.storage_quota <= 0"
              @click="openIncreaseDialog(row)"
            >
              {{ t('systemTenants.increase.action') }}
            </t-button>
          </template>
          <template #empty>{{ t('systemTenants.empty') }}</template>
        </t-table>
      </div>
      <div v-if="total > 0" class="tenants-pager">
        <t-pagination
          v-model="page"
          v-model:page-size="pageSize"
          :total="total"
          :page-size-options="[10, 20, 50]"
          size="small"
          show-jumper
          show-page-size
          @change="loadTenants"
        />
      </div>
    </div>

    <t-dialog
      v-model:visible="increaseVisible"
      :header="t('systemTenants.increase.action')"
      width="480px"
      :confirm-btn="{ content: t('common.confirm'), loading: submitting, disabled: !validIncrease || submitting }"
      :cancel-btn="{ content: t('common.cancel'), disabled: submitting }"
      :close-on-overlay-click="!submitting"
      :close-on-esc-keydown="!submitting"
      :close-btn="!submitting"
      @confirm="submitIncrease"
    >
      <div v-if="increaseTarget" class="increase-form">
        <div class="identity">
          <strong>{{ increaseTarget.name }}</strong>
          <span class="secondary">ID: {{ increaseTarget.id }}</span>
        </div>
        <div class="quota-summary">
          <span>{{ t('systemTenants.increase.current') }}</span>
          <strong>{{ formatCapacity(increaseTarget.storage_quota) }}</strong>
        </div>
        <label for="quota-increase">{{ t('systemTenants.increase.amount') }}</label>
        <t-input-number
          id="quota-increase"
          v-model="increaseGB"
          :min="1"
          :max="8589934591"
          :step="1"
          :decimal-places="0"
          :disabled="submitting"
        />
        <div class="quota-summary">
          <span>{{ t('systemTenants.increase.after') }}</span>
          <strong>{{ validIncrease ? formatCapacity(increaseTarget.storage_quota + Number(increaseGB) * BYTES_PER_GB) : '—' }}</strong>
        </div>
        <p class="secondary">{{ t('systemTenants.increase.hint') }}</p>
      </div>
    </t-dialog>
  </div>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { MessagePlugin } from 'tdesign-vue-next'
import { increaseTenantStorageQuota, listSystemTenants, type SystemTenant } from '@/api/system'

const { t, locale } = useI18n()
const BYTES_PER_GB = 1024 ** 3
const tenants = ref<SystemTenant[]>([])
const total = ref(0)
const page = ref(1)
const pageSize = ref(20)
const searchInput = ref('')
const searchQuery = ref('')
const loading = ref(false)
const error = ref('')
let loadRequestId = 0

const columns = computed(() => [
  { colKey: 'workspace', title: t('systemTenants.columns.workspace'), minWidth: 180 },
  { colKey: 'owners', title: t('systemTenants.columns.owners'), minWidth: 210 },
  { colKey: 'storage_used', title: t('systemTenants.columns.used'), width: 130 },
  { colKey: 'storage_quota', title: t('systemTenants.columns.quota'), width: 130 },
  { colKey: 'actions', title: t('systemTenants.columns.actions'), width: 130 },
])

function formatCapacity(bytes: number): string {
  return `${new Intl.NumberFormat(locale.value, { maximumFractionDigits: 3 }).format(bytes / BYTES_PER_GB)} GB`
}

async function loadTenants() {
  const requestId = ++loadRequestId
  loading.value = true
  error.value = ''
  try {
    const response = await listSystemTenants({
      query: searchQuery.value,
      page: page.value,
      page_size: pageSize.value,
    })
    if (requestId !== loadRequestId) return
    tenants.value = response.tenants
    total.value = response.total
  } catch (err: any) {
    if (requestId === loadRequestId) error.value = err.message
  } finally {
    if (requestId === loadRequestId) loading.value = false
  }
}

function searchTenants() {
  searchQuery.value = searchInput.value.trim()
  page.value = 1
  void loadTenants()
}

const increaseVisible = ref(false)
const increaseTarget = ref<SystemTenant | null>(null)
const increaseGB = ref<number | string>(1)
const submitting = ref(false)
const validIncrease = computed(() =>
  typeof increaseGB.value === 'number' && Number.isInteger(increaseGB.value)
  && increaseGB.value > 0 && increaseGB.value <= 8589934591,
)

function openIncreaseDialog(tenant: SystemTenant) {
  increaseTarget.value = tenant
  increaseGB.value = 1
  increaseVisible.value = true
}

async function submitIncrease() {
  if (submitting.value || !validIncrease.value || !increaseTarget.value) return
  submitting.value = true
  try {
    const response = await increaseTenantStorageQuota(increaseTarget.value.id, Number(increaseGB.value))
    increaseVisible.value = false
    MessagePlugin.success(t('systemTenants.increase.success', { quota: formatCapacity(response.storage_quota) }))
    await loadTenants()
  } catch (err: any) {
    MessagePlugin.error(err.message)
  } finally {
    submitting.value = false
  }
}

onMounted(loadTenants)
onBeforeUnmount(() => { loadRequestId++ })
</script>

<style scoped lang="less">
.system-tenants { color: var(--td-text-color-primary); }
.section-header {
  margin-bottom: 24px;
  h2 { margin: 0 0 8px; font-size: 22px; line-height: 1.3; }
  p { margin: 0; color: var(--td-text-color-secondary); line-height: 1.6; }
}
.tenants-toolbar, .tenants-search, .quota-summary {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
}
.tenants-toolbar { margin-bottom: 14px; flex-wrap: wrap; }
.tenants-search { width: min(440px, 100%); }
.tenants-count {
  display: flex;
  align-items: baseline;
  gap: 6px;
  color: var(--td-text-color-secondary);
  strong { font-size: 20px; color: var(--td-text-color-primary); }
}
.tenants-table-shell {
  overflow: hidden;
  border: 1px solid var(--td-component-stroke);
  border-radius: 10px;
  background: var(--td-bg-color-container);
}
.tenants-table-scroll { overflow-x: auto; }
.tenants-pager {
  display: flex;
  justify-content: flex-end;
  padding: 12px 14px;
  border-top: 1px solid var(--td-component-stroke);
}
.identity { display: flex; flex-direction: column; gap: 3px; overflow-wrap: anywhere; }
.workspace-name { font-weight: 500; }
.owner + .owner { margin-top: 8px; }
.secondary { color: var(--td-text-color-secondary); font-size: 12px; }
.increase-form {
  display: flex;
  flex-direction: column;
  gap: 16px;
  p { margin: 0; line-height: 1.6; }
  :deep(.t-input-number) { width: 100%; }
}
</style>
