<template>
  <div class="list-space-sidebar">
    <nav class="expanded-panel">
      <div v-if="!hideAll" class="sidebar-item" :class="{ active: selected === 'all' }" @click="select('all')">
        <div class="item-left">
          <t-icon name="layers" class="item-icon" />
          <span class="item-label">{{ $t('listSpaceSidebar.all') }}</span>
        </div>
        <span v-if="countAll !== undefined" class="item-count">{{ countAll }}</span>
      </div>

      <template v-if="mode === 'resource'">
        <div v-if="showFavorites" class="sidebar-item" :class="{ active: selected === 'favorites' }"
          @click="select('favorites')">
          <div class="item-left">
            <t-icon name="star" class="item-icon" />
            <span class="item-label">{{ $t('listSpaceSidebar.favorites') }}</span>
          </div>
          <span v-if="countFavorites > 0" class="item-count">{{ countFavorites }}</span>
        </div>
        <div v-if="showRecents" class="sidebar-item" :class="{ active: selected === 'recents' }"
          @click="select('recents')">
          <div class="item-left">
            <t-icon name="history" class="item-icon" />
            <span class="item-label">{{ $t('listSpaceSidebar.recents') }}</span>
          </div>
          <span v-if="countRecents > 0" class="item-count">{{ countRecents }}</span>
        </div>
        <div v-if="(showFavorites || showRecents)" class="sidebar-divider" />
        <div class="sidebar-item" :class="{ active: selected === 'mine' }" @click="select('mine')">
          <div class="item-left">
            <t-icon name="system-sum" class="item-icon" />
            <span class="item-label">{{ workspaceLabel }}</span>
          </div>
          <span v-if="countMine !== undefined" class="item-count">{{ countMine }}</span>
        </div>
        <!-- Shared spaces group — per-org entries only. -->
        <template v-if="organizationsWithCount.length">
          <div class="sidebar-section">
            <span class="section-title">{{ $t('listSpaceSidebar.spaces') }}</span>
          </div>
          <div v-for="org in organizationsWithCount" :key="org.id" class="sidebar-item org-item"
            :class="{ active: selected === org.id }" @click="select(org.id)">
            <div class="item-left">
              <SpaceAvatar :name="org.name" :avatar="org.avatar" size="small" class="item-avatar" />
              <span class="item-label" :title="org.name">{{ org.name }}</span>
            </div>
            <span v-if="getOrgCount(org.id) !== undefined" class="item-count">{{ getOrgCount(org.id) }}</span>
          </div>
        </template>
      </template>

      <template v-else>
        <div class="sidebar-item" :class="{ active: selected === 'created' }" @click="select('created')">
          <div class="item-left">
            <t-icon name="usergroup-add" class="item-icon" />
            <span class="item-label">{{ $t('organization.createdByMe') }}</span>
          </div>
          <span v-if="countCreated !== undefined" class="item-count">{{ countCreated }}</span>
        </div>
        <div class="sidebar-item" :class="{ active: selected === 'joined' }" @click="select('joined')">
          <div class="item-left">
            <t-icon name="usergroup" class="item-icon" />
            <span class="item-label">{{ $t('organization.joinedByMe') }}</span>
          </div>
          <span v-if="countJoined !== undefined" class="item-count">{{ countJoined }}</span>
        </div>
      </template>
    </nav>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted } from 'vue'
import { useI18n } from 'vue-i18n'
import { Icon as TIcon } from 'tdesign-vue-next'
import SpaceAvatar from './SpaceAvatar.vue'
import { useOrganizationStore } from '@/stores/organization'

const props = withDefaults(
  defineProps<{
    mode?: 'resource' | 'organization'
    modelValue: string
    countAll?: number
    countMine?: number
    countByOrg?: Record<string, number>
    countCreated?: number
    countJoined?: number
    hideAll?: boolean
    /** Favorites entry. Only meaningful in resource mode. */
    countFavorites?: number
    showFavorites?: boolean
    /** Recents entry. Only meaningful in resource mode. */
    countRecents?: number
    showRecents?: boolean
  }>(),
  {
    mode: 'resource',
    countAll: undefined,
    countMine: undefined,
    countByOrg: () => ({}),
    countCreated: undefined,
    countJoined: undefined,
    hideAll: false,
    countFavorites: 0,
    showFavorites: true,
    countRecents: 0,
    showRecents: true,
  }
)

const emit = defineEmits<{
  'update:modelValue': [value: string]
}>()

const orgStore = useOrganizationStore()
const { t } = useI18n()
const selected = computed({
  get: () => props.modelValue,
  set: (v: string) => emit('update:modelValue', v)
})

// The tenant identity is already shown by TenantSelector in the global header,
// so this navigation uses a concise, stable label for the tenant-owned bucket.
const workspaceLabel = computed(() => t('listSpaceSidebar.workspace'))

const organizations = computed(() => orgStore.organizations || [])

const organizationsWithCount = computed(() => {
  if (props.mode !== 'resource') return organizations.value
  return organizations.value.filter((org) => (props.countByOrg?.[org.id] ?? 0) > 0)
})

function select(value: string) {
  selected.value = value
}

function getOrgCount(orgId: string): number | undefined {
  const n = props.countByOrg?.[orgId]
  return n === undefined ? undefined : n
}

onMounted(() => {
  orgStore.fetchOrganizations()
})
</script>

<style scoped lang="less">
.list-space-sidebar {
  width: 208px;
  flex-shrink: 0;
  display: flex;
  flex-direction: column;
  min-height: 0;
  z-index: 10;
}

/* ========== Expanded panel ========== */
.expanded-panel {
  display: flex;
  flex-direction: column;
  gap: 2px;
  padding: 12px 8px;
  flex: 1;
  min-height: 0;
  overflow-y: auto;
  overflow-x: hidden;
  scrollbar-width: none;
  border-right: 1px solid var(--td-component-stroke);

  &::-webkit-scrollbar {
    display: none;
  }
}

/* ========== Nav items inside expanded panel ========== */
.sidebar-item {
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 6px 8px;
  border-radius: 7px;
  color: var(--td-text-color-primary);
  cursor: pointer;
  transition: all 0.15s ease;
  font-family: var(--app-font-family);
  font-size: 14px;
  -webkit-font-smoothing: antialiased;

  .item-left {
    display: flex;
    align-items: center;
    gap: 6px;
    min-width: 0;
    flex: 1;
  }

  .item-icon {
    flex-shrink: 0;
    color: var(--td-text-color-secondary);
    font-size: 14px;
    transition: color 0.15s ease;
  }

  .item-avatar {
    flex-shrink: 0;
  }

  .item-label {
    flex: 1;
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    font-size: 13px;
    font-weight: 430;
    line-height: 1.4;
    letter-spacing: 0.01em;
  }

  .item-count {
    font-size: 12px;
    color: var(--td-text-color-secondary);
    font-weight: 500;
    padding: 2px 7px;
    border-radius: 8px;
    background: var(--td-bg-color-secondarycontainer);
    margin-left: 6px;
    flex-shrink: 0;
    transition: all 0.15s ease;
  }

  &:hover {
    background: var(--td-bg-color-container-hover);
    color: var(--td-text-color-primary);

    .item-icon {
      color: var(--td-text-color-primary);
    }

    .item-count {
      background: var(--td-bg-color-secondarycontainer);
      color: var(--td-text-color-primary);
    }
  }

  &.active {
    background: var(--td-bg-color-secondarycontainer);
    color: var(--td-brand-color);

    .item-icon {
      color: var(--td-brand-color);
    }

    .item-count {
      background: var(--td-bg-color-secondarycontainer);
      color: var(--td-brand-color);
    }

    &:hover {
      background: var(--td-bg-color-secondarycontainer);
    }
  }
}

.sidebar-divider {
  height: 1px;
  margin: 6px 4px;
  background: var(--td-component-stroke);
}

.sidebar-section {
  padding: 8px 6px 2px;
  margin-top: 2px;
  border-top: 1px solid var(--td-component-stroke);

  .section-title {
    font-size: 12px;
    color: var(--td-text-color-secondary);
    font-weight: 600;
    line-height: 1.4;
  }
}

@media (max-width: 768px) {
  .list-space-sidebar {
    width: 100%;
    min-width: 0;
  }

  .expanded-panel {
    flex-direction: row;
    flex: none;
    padding: 8px 12px;
    overflow-x: auto;
    overflow-y: hidden;
    border-right: 0;
    border-bottom: 1px solid var(--td-component-stroke);
  }

  .sidebar-item {
    flex-shrink: 0;
  }

  .sidebar-divider {
    width: 1px;
    height: 24px;
    margin: 2px 4px;
    flex-shrink: 0;
  }

  .sidebar-section {
    display: flex;
    align-items: center;
    padding: 0 4px 0 10px;
    margin: 2px 0;
    border-top: 0;
    border-left: 1px solid var(--td-component-stroke);
    flex-shrink: 0;
  }
}
</style>
