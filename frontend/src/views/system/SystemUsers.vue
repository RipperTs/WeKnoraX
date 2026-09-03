<template>
  <div class="system-users">
    <div class="section-header">
      <div>
        <h2>{{ t('systemUsers.title') }}</h2>
        <p class="section-description">{{ t('systemUsers.description') }}</p>
      </div>
      <t-button
        type="button"
        theme="primary"
        variant="text"
        size="medium"
        class="section-action-trigger"
        @click="openCreateDialog"
      >
        <template #icon><t-icon name="user-add" /></template>
        {{ t('systemUsers.create.action') }}
      </t-button>
    </div>

    <div class="users-toolbar">
      <div class="users-count">
        <span class="users-count__value">{{ total }}</span>
        <span>{{ t('systemUsers.total') }}</span>
      </div>
      <div class="users-filters">
        <t-input
          v-model="searchInput"
          class="users-search"
          clearable
          :placeholder="t('systemUsers.searchPlaceholder')"
        >
          <template #prefix-icon><t-icon name="search" /></template>
        </t-input>
        <t-select
          v-model="statusFilter"
          class="status-filter"
          :options="statusOptions"
        />
        <t-tooltip :content="t('systemUsers.refresh')">
          <t-button
            shape="square"
            variant="outline"
            :loading="loading"
            :aria-label="t('systemUsers.refresh')"
            @click="loadUsers"
          >
            <template #icon><t-icon name="refresh" /></template>
          </t-button>
        </t-tooltip>
      </div>
    </div>

    <div v-if="error" class="users-error">
      <t-alert theme="error" :message="error">
        <template #operation>
          <t-button size="small" @click="loadUsers">{{ t('systemUsers.retry') }}</t-button>
        </template>
      </t-alert>
    </div>

    <div v-else-if="!loading && total === 0" class="users-empty">
      <t-empty
        :description="searchQuery || statusFilter !== 'all'
          ? t('systemUsers.emptyFiltered')
          : t('systemUsers.empty')"
      />
    </div>

    <div v-else class="users-table-shell">
      <div class="users-table-scroll">
        <t-table
          row-key="id"
          :data="users"
          :columns="columns"
          :loading="loading"
          size="medium"
          hover
        >
          <template #user="{ row }">
            <div class="user-cell">
              <div class="user-avatar" aria-hidden="true">{{ avatarLetter(row) }}</div>
              <div class="user-identity">
                <div class="user-name-line">
                  <span class="user-name">{{ userPrimary(row) }}</span>
                  <span v-if="row.id === currentUserId" class="self-badge">{{ t('common.me') }}</span>
                </div>
                <span class="user-email" :title="row.email">{{ row.email }}</span>
              </div>
            </div>
          </template>

          <template #status="{ row }">
            <t-tag :theme="row.is_active ? 'success' : 'default'" variant="light" size="small">
              {{ row.is_active ? t('systemUsers.status.active') : t('systemUsers.status.disabled') }}
            </t-tag>
          </template>

          <template #role="{ row }">
            <t-tag v-if="row.is_system_admin" theme="warning" variant="light" size="small">
              {{ t('systemUsers.role.systemAdmin') }}
            </t-tag>
            <span v-else class="regular-user-role">{{ t('systemUsers.role.regular') }}</span>
          </template>

          <template #created_at="{ row }">
            <span class="created-at">{{ formatDate(row.created_at) }}</span>
          </template>

          <template #actions="{ row }">
            <t-dropdown
              trigger="click"
              placement="bottom-right"
              attach="body"
              :options="actionOptions(row)"
              @click="(data: any) => handleAction(data.value, row)"
            >
              <t-button
                shape="square"
                variant="text"
                size="small"
                :loading="operationUserId === row.id"
                :disabled="operationUserId === row.id"
                :aria-label="t('systemUsers.actions.more')"
              >
                <template #icon><t-icon name="ellipsis" /></template>
              </t-button>
            </t-dropdown>
          </template>
        </t-table>
      </div>
      <div v-if="total > 0" class="users-pager">
        <t-pagination
          v-model="page"
          v-model:page-size="pageSize"
          :total="total"
          :page-size-options="PAGE_SIZE_OPTIONS"
          size="small"
          show-jumper
          show-page-number
          show-page-size
          @change="loadUsers"
        />
      </div>
    </div>

    <t-dialog
      v-model:visible="createVisible"
      :header="t('systemUsers.create.dialogTitle')"
      width="480px"
      placement="center"
      :confirm-btn="{
        content: t('systemUsers.create.confirm'),
        loading: createSubmitting,
      }"
      :cancel-btn="t('common.cancel')"
      :close-on-overlay-click="!createSubmitting"
      :close-btn="!createSubmitting"
      @confirm="submitCreate"
      @close="resetCreateForm"
    >
      <t-form
        ref="createFormRef"
        :data="createForm"
        :rules="createRules"
        label-align="top"
      >
        <t-form-item :label="t('systemUsers.create.username')" name="username">
          <t-input
            v-model="createForm.username"
            :disabled="createSubmitting"
            :placeholder="t('systemUsers.create.usernamePlaceholder')"
            autocomplete="off"
          />
        </t-form-item>
        <t-form-item :label="t('systemUsers.create.email')" name="email">
          <t-input
            v-model="createForm.email"
            :disabled="createSubmitting"
            :placeholder="t('systemUsers.create.emailPlaceholder')"
            autocomplete="off"
          />
        </t-form-item>
        <t-form-item :label="t('systemUsers.create.passwordMode')" name="passwordMode">
          <t-radio-group v-model="createForm.passwordMode" :disabled="createSubmitting">
            <t-radio value="generated">{{ t('systemUsers.create.generated') }}</t-radio>
            <t-radio value="manual">{{ t('systemUsers.create.manual') }}</t-radio>
          </t-radio-group>
        </t-form-item>
        <t-alert
          v-if="createForm.passwordMode === 'generated'"
          theme="info"
          :message="t('systemUsers.create.generatedHint')"
          class="form-alert"
        />
        <template v-else>
          <t-form-item :label="t('systemUsers.password.newPassword')" name="password">
            <t-input
              v-model="createForm.password"
              type="password"
              autocomplete="new-password"
              :disabled="createSubmitting"
              :placeholder="t('systemUsers.password.placeholder')"
            />
          </t-form-item>
          <t-form-item :label="t('systemUsers.password.confirmPassword')" name="confirmPassword">
            <t-input
              v-model="createForm.confirmPassword"
              type="password"
              autocomplete="new-password"
              :disabled="createSubmitting"
              :placeholder="t('systemUsers.password.confirmPlaceholder')"
              @enter="submitCreate"
            />
          </t-form-item>
        </template>
      </t-form>
    </t-dialog>

    <t-dialog
      v-model:visible="passwordResultVisible"
      :header="t('systemUsers.create.passwordResultTitle')"
      width="480px"
      placement="center"
      :footer="false"
      :close-on-overlay-click="false"
      @close="clearGeneratedPassword"
    >
      <t-alert
        class="password-result-warning"
        theme="warning"
        :message="t('systemUsers.create.passwordResultWarning')"
      />
      <div class="generated-password">
        <div class="generated-password__header">
          <span class="generated-password__label">
            {{ t('systemUsers.create.passwordMode') }}
          </span>
          <t-button
            class="generated-password__copy"
            theme="primary"
            variant="text"
            size="small"
            @click="copyGeneratedPassword"
          >
            <template #icon><t-icon name="file-copy" /></template>
            {{ t('systemUsers.create.copyPassword') }}
          </t-button>
        </div>
        <code>{{ generatedPassword }}</code>
      </div>
    </t-dialog>

    <t-dialog
      v-model:visible="resetVisible"
      :header="t('systemUsers.password.dialogTitle')"
      width="460px"
      placement="center"
      :confirm-btn="{
        content: t('systemUsers.password.confirm'),
        theme: 'danger',
        loading: resetSubmitting,
      }"
      :cancel-btn="t('common.cancel')"
      :close-on-overlay-click="!resetSubmitting"
      :close-btn="!resetSubmitting"
      @confirm="submitPasswordReset"
      @close="resetPasswordForm"
    >
      <t-alert
        theme="warning"
        :message="t('systemUsers.password.warning', { email: resetTarget?.email || '' })"
        class="form-alert"
      />
      <t-form
        ref="resetFormRef"
        :data="resetForm"
        :rules="passwordRules"
        label-align="top"
      >
        <t-form-item :label="t('systemUsers.password.newPassword')" name="password">
          <t-input
            v-model="resetForm.password"
            type="password"
            autocomplete="new-password"
            :disabled="resetSubmitting"
            :placeholder="t('systemUsers.password.placeholder')"
          />
        </t-form-item>
        <t-form-item :label="t('systemUsers.password.confirmPassword')" name="confirmPassword">
          <t-input
            v-model="resetForm.confirmPassword"
            type="password"
            autocomplete="new-password"
            :disabled="resetSubmitting"
            :placeholder="t('systemUsers.password.confirmPlaceholder')"
            @enter="submitPasswordReset"
          />
        </t-form-item>
      </t-form>
    </t-dialog>
  </div>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, reactive, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { DialogPlugin, MessagePlugin } from 'tdesign-vue-next'
import type { FormInstanceFunctions, FormRule } from 'tdesign-vue-next'
import {
  createSystemUser,
  listSystemUsers,
  promoteUserToSystemAdmin,
  resetUserPassword,
  revokeSystemAdmin,
  updateSystemUserStatus,
  type SystemAdminUser,
  type SystemUserStatusFilter,
} from '@/api/system'
import { useAuthStore } from '@/stores/auth'
import { copyToClipboard } from '@/utils/clipboard'

type UserAction = 'reset-password' | 'promote' | 'revoke' | 'enable' | 'disable'
type PasswordMode = 'generated' | 'manual'

const { t, locale } = useI18n()
const authStore = useAuthStore()
const currentUserId = computed(() => authStore.currentUserId)

const users = ref<SystemAdminUser[]>([])
const total = ref(0)
const page = ref(1)
const pageSize = ref(20)
const searchInput = ref('')
const searchQuery = ref('')
const statusFilter = ref<SystemUserStatusFilter>('all')
const loading = ref(false)
const error = ref('')
const operationUserId = ref('')
const PAGE_SIZE_OPTIONS = [10, 20, 50]
let searchTimer: ReturnType<typeof setTimeout> | null = null
let loadRequestId = 0

const statusOptions = computed(() => [
  { label: t('systemUsers.status.all'), value: 'all' },
  { label: t('systemUsers.status.active'), value: 'active' },
  { label: t('systemUsers.status.disabled'), value: 'disabled' },
])

const columns = computed(() => [
  { colKey: 'user', title: t('systemUsers.columns.user'), minWidth: 220, ellipsis: true },
  { colKey: 'status', title: t('systemUsers.columns.status'), width: 92 },
  { colKey: 'role', title: t('systemUsers.columns.role'), width: 116 },
  { colKey: 'created_at', title: t('systemUsers.columns.createdAt'), width: 142 },
  { colKey: 'actions', title: t('systemUsers.columns.actions'), width: 60, align: 'center' as const },
])

async function loadUsers() {
  const requestId = ++loadRequestId
  loading.value = true
  error.value = ''
  try {
    const response = await listSystemUsers({
      query: searchQuery.value,
      status: statusFilter.value,
      page: page.value,
      page_size: pageSize.value,
    })
    if (requestId !== loadRequestId) return
    users.value = response.users ?? []
    total.value = response.total ?? 0
    const lastPage = Math.max(1, Math.ceil(total.value / pageSize.value))
    if (page.value > lastPage) {
      page.value = lastPage
      await loadUsers()
    }
  } catch (err: any) {
    if (requestId !== loadRequestId) return
    error.value = err?.message || t('systemUsers.loadFailed')
  } finally {
    if (requestId === loadRequestId) loading.value = false
  }
}

watch(searchInput, (value) => {
  if (searchTimer) clearTimeout(searchTimer)
  searchTimer = setTimeout(() => {
    searchQuery.value = value.trim()
    page.value = 1
    void loadUsers()
  }, 320)
})

watch(statusFilter, () => {
  page.value = 1
  void loadUsers()
})

onMounted(loadUsers)
onBeforeUnmount(() => {
  if (searchTimer) clearTimeout(searchTimer)
  loadRequestId++
})

function userPrimary(user: SystemAdminUser): string {
  return user.name?.trim() || user.username?.trim() || user.email
}

function avatarLetter(user: SystemAdminUser): string {
  return userPrimary(user).slice(0, 1).toUpperCase() || '?'
}

function formatDate(value: string): string {
  if (!value) return '—'
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return '—'
  return new Intl.DateTimeFormat(locale.value, {
    year: 'numeric',
    month: '2-digit',
    day: '2-digit',
  }).format(date)
}

function actionOptions(user: SystemAdminUser) {
  const isSelf = user.id === currentUserId.value
  const statusAction = user.is_active
    ? {
        content: user.is_system_admin
          ? t('systemUsers.actions.revokeBeforeDisable')
          : t('systemUsers.actions.disable'),
        value: 'disable',
        disabled: isSelf || user.is_system_admin,
      }
    : { content: t('systemUsers.actions.enable'), value: 'enable' }
  return [
    {
      content: t('systemUsers.actions.resetPassword'),
      value: 'reset-password',
      disabled: isSelf,
    },
    user.is_system_admin
      ? {
          content: t('systemUsers.actions.revokeAdmin'),
          value: 'revoke',
          disabled: isSelf,
        }
      : { content: t('systemUsers.actions.promoteAdmin'), value: 'promote' },
    statusAction,
  ]
}

function handleAction(action: UserAction, user: SystemAdminUser) {
  if (action === 'reset-password') {
    openPasswordReset(user)
    return
  }
  const key = action === 'promote' || action === 'revoke' ? action : action === 'enable' ? 'enable' : 'disable'
  const dialog = DialogPlugin.confirm({
    header: t(`systemUsers.confirm.${key}.title`),
    body: t(`systemUsers.confirm.${key}.body`, { email: user.email }),
    confirmBtn: {
      content: t(`systemUsers.confirm.${key}.confirm`),
      theme: action === 'disable' || action === 'revoke' ? 'danger' : 'primary',
    },
    cancelBtn: t('common.cancel'),
    onConfirm: () => {
      dialog.destroy()
      void runUserAction(action, user)
    },
    onCancel: () => dialog.destroy(),
  })
}

async function runUserAction(action: Exclude<UserAction, 'reset-password'>, user: SystemAdminUser) {
  operationUserId.value = user.id
  try {
    if (action === 'promote') {
      await promoteUserToSystemAdmin({ user_id: user.id })
    } else if (action === 'revoke') {
      await revokeSystemAdmin(user.id)
    } else {
      await updateSystemUserStatus(user.id, action === 'enable')
    }
    MessagePlugin.success(t(`systemUsers.messages.${action}Success`))
    await loadUsers()
  } catch (err: any) {
    MessagePlugin.error(err?.message || t(`systemUsers.messages.${action}Failed`))
  } finally {
    operationUserId.value = ''
  }
}

const createVisible = ref(false)
const createSubmitting = ref(false)
const createFormRef = ref<FormInstanceFunctions>()
const createForm = reactive({
  username: '',
  email: '',
  passwordMode: 'generated' as PasswordMode,
  password: '',
  confirmPassword: '',
})

const basePasswordRules: FormRule[] = [
  { required: true, message: t('systemUsers.validation.passwordRequired'), trigger: 'blur' },
  { min: 8, message: t('systemUsers.validation.passwordLength'), trigger: 'blur' },
  { max: 32, message: t('systemUsers.validation.passwordLength'), trigger: 'blur' },
  { pattern: /[a-zA-Z]/, message: t('systemUsers.validation.passwordLetter'), trigger: 'blur' },
  { pattern: /\d/, message: t('systemUsers.validation.passwordNumber'), trigger: 'blur' },
]

const createRules = computed<Record<string, FormRule[]>>(() => ({
  username: [
    { required: true, message: t('systemUsers.validation.usernameRequired'), trigger: 'blur' },
    { min: 2, message: t('systemUsers.validation.usernameLength'), trigger: 'blur' },
    { max: 50, message: t('systemUsers.validation.usernameLength'), trigger: 'blur' },
    { pattern: /^[^@]+$/, message: t('systemUsers.validation.usernameAt'), trigger: 'blur' },
  ],
  email: [
    { required: true, message: t('systemUsers.validation.emailRequired'), trigger: 'blur' },
    { email: true, message: t('systemUsers.validation.emailInvalid'), trigger: 'blur' },
  ],
  password: createForm.passwordMode === 'manual' ? basePasswordRules : [],
  confirmPassword: createForm.passwordMode === 'manual'
    ? [
        { required: true, message: t('systemUsers.validation.confirmRequired'), trigger: 'blur' },
        {
          validator: (value: string) => value === createForm.password,
          message: t('systemUsers.validation.passwordMismatch'),
          trigger: 'blur',
        },
      ]
    : [],
}))

function openCreateDialog() {
  resetCreateForm()
  createVisible.value = true
}

function resetCreateForm() {
  createForm.username = ''
  createForm.email = ''
  createForm.passwordMode = 'generated'
  createForm.password = ''
  createForm.confirmPassword = ''
  createFormRef.value?.clearValidate?.()
}

const passwordResultVisible = ref(false)
const generatedPassword = ref('')

async function submitCreate() {
  if (createSubmitting.value) return
  const valid = await createFormRef.value?.validate?.()
  if (valid !== true) return

  createSubmitting.value = true
  try {
    const response = await createSystemUser({
      username: createForm.username.trim(),
      email: createForm.email.trim(),
      ...(createForm.passwordMode === 'manual' ? { password: createForm.password } : {}),
    })
    createVisible.value = false
    generatedPassword.value = response.generated_password || ''
    if (generatedPassword.value) {
      passwordResultVisible.value = true
    } else {
      MessagePlugin.success(t('systemUsers.create.success'))
    }
    page.value = 1
    await loadUsers()
  } catch (err: any) {
    MessagePlugin.error(err?.message || t('systemUsers.create.failed'))
  } finally {
    createSubmitting.value = false
  }
}

async function copyGeneratedPassword() {
  const copied = await copyToClipboard(generatedPassword.value)
  if (copied) {
    MessagePlugin.success(t('systemUsers.create.copied'))
  } else {
    MessagePlugin.error(t('common.copyFailed'))
  }
}

function clearGeneratedPassword() {
  generatedPassword.value = ''
}

const resetVisible = ref(false)
const resetSubmitting = ref(false)
const resetTarget = ref<SystemAdminUser | null>(null)
const resetFormRef = ref<FormInstanceFunctions>()
const resetForm = reactive({ password: '', confirmPassword: '' })
const passwordRules: Record<string, FormRule[]> = {
  password: basePasswordRules,
  confirmPassword: [
    { required: true, message: t('systemUsers.validation.confirmRequired'), trigger: 'blur' },
    {
      validator: (value: string) => value === resetForm.password,
      message: t('systemUsers.validation.passwordMismatch'),
      trigger: 'blur',
    },
  ],
}

function openPasswordReset(user: SystemAdminUser) {
  resetPasswordForm()
  resetTarget.value = user
  resetVisible.value = true
}

function resetPasswordForm() {
  resetForm.password = ''
  resetForm.confirmPassword = ''
  resetTarget.value = null
  resetFormRef.value?.clearValidate?.()
}

async function submitPasswordReset() {
  if (resetSubmitting.value || !resetTarget.value) return
  const valid = await resetFormRef.value?.validate?.()
  if (valid !== true) return

  resetSubmitting.value = true
  try {
    await resetUserPassword({
      email: resetTarget.value.email,
      new_password: resetForm.password,
    })
    resetVisible.value = false
    MessagePlugin.success(t('systemUsers.password.success'))
  } catch (err: any) {
    MessagePlugin.error(err?.message || t('systemUsers.password.failed'))
  } finally {
    resetSubmitting.value = false
  }
}
</script>

<style scoped lang="less">
.system-users {
  color: var(--td-text-color-primary);
}

.section-header {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 24px;
  margin-bottom: 24px;

  h2 {
    margin: 0 0 8px;
    font-size: 22px;
    line-height: 1.3;
  }
}

.section-action-trigger {
  --td-bg-color-container-hover: transparent;
  flex-shrink: 0;
  padding-left: 0;
  padding-right: 0;
  font-weight: 600;

  &:hover,
  &:focus,
  &.t-is-active,
  &:active {
    background-color: transparent !important;
    color: var(--td-brand-color-hover);
  }

  &:active {
    color: var(--td-brand-color-active);
  }
}

.section-description {
  max-width: 620px;
  margin: 0;
  color: var(--td-text-color-secondary);
  font-size: 14px;
  line-height: 1.6;
}

.users-toolbar {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 16px;
  margin-bottom: 14px;
}

.users-count {
  display: flex;
  align-items: baseline;
  gap: 6px;
  color: var(--td-text-color-secondary);
  font-size: 13px;

  &__value {
    color: var(--td-text-color-primary);
    font-size: 20px;
    font-weight: 600;
    font-variant-numeric: tabular-nums;
  }
}

.users-filters {
  display: flex;
  align-items: center;
  gap: 8px;
}

.users-search {
  width: 250px;
}

.status-filter {
  width: 116px;
}

.users-table-shell {
  overflow: hidden;
  border: 1px solid var(--td-component-stroke);
  border-radius: 10px;
  background: var(--td-bg-color-container);
}

.users-table-scroll {
  overflow-x: auto;
}

.users-pager {
  display: flex;
  justify-content: flex-end;
  padding: 12px 14px;
  border-top: 1px solid var(--td-component-stroke);
  background: var(--td-bg-color-secondarycontainer);
}

.user-cell {
  display: flex;
  min-width: 0;
  align-items: center;
  gap: 10px;
}

.user-avatar {
  display: grid;
  width: 34px;
  height: 34px;
  flex: 0 0 34px;
  place-items: center;
  border: 1px solid var(--td-brand-color-light);
  border-radius: 9px;
  background: var(--td-brand-color-light);
  color: var(--td-brand-color);
  font-size: 14px;
  font-weight: 600;
}

.user-identity {
  display: flex;
  min-width: 0;
  flex-direction: column;
  gap: 3px;
}

.user-name-line {
  display: flex;
  min-width: 0;
  align-items: center;
  gap: 6px;
}

.user-name,
.user-email {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.user-name {
  font-size: 14px;
  font-weight: 500;
}

.user-email,
.created-at,
.regular-user-role {
  color: var(--td-text-color-secondary);
  font-size: 12px;
}

.self-badge {
  flex: 0 0 auto;
  padding: 1px 5px;
  border-radius: 4px;
  background: var(--td-bg-color-secondarycontainer);
  color: var(--td-text-color-secondary);
  font-size: 11px;
}

.created-at {
  font-variant-numeric: tabular-nums;
}

.users-error,
.users-empty {
  padding: 36px 0;
}

.form-alert {
  margin-bottom: 18px;
}

.password-result-warning {
  margin-bottom: 16px;
}

.generated-password {
  overflow: hidden;
  border: 1px solid var(--td-component-stroke);
  border-radius: 10px;
  background: var(--td-bg-color-secondarycontainer);

  &__header {
    display: flex;
    min-height: 44px;
    align-items: center;
    justify-content: space-between;
    gap: 12px;
    padding: 0 12px 0 16px;
    border-bottom: 1px solid var(--td-component-stroke);
    background: var(--td-bg-color-container);
  }

  &__label {
    color: var(--td-text-color-secondary);
    font-size: 13px;
    font-weight: 500;
  }

  &__copy {
    flex: 0 0 auto;
  }

  code {
    display: block;
    padding: 18px 16px;
    color: var(--td-text-color-primary);
    font-size: 15px;
    line-height: 1.6;
    overflow-wrap: anywhere;
    user-select: all;
  }
}

@media (max-width: 860px) {
  .users-toolbar,
  .section-header {
    align-items: stretch;
    flex-direction: column;
  }

  .users-filters {
    flex-wrap: wrap;
  }

  .users-search {
    min-width: 210px;
    flex: 1;
    width: auto;
  }
}
</style>
