<script setup>
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'

// 白 / 暗 / 自动 三态切换。
//
// 为什么不用主题自带的那个按钮：VuePress 2 的原生开关只有两态
//（亮 ⇄ 暗），「自动」只是首次访问时的取值来源（prefers-color-scheme），
// 之后用户手动切过就再也回不到「跟随系统」。主题内部的取值模型本来就有
// 三态（localStorage['vuepress-color-scheme'] ∈ auto|light|dark），只是
// 界面没给出入口——所以这里接的是同一份存储，不是另造一套。
const STORAGE_KEY = 'vuepress-color-scheme'

const MODES = [
  { value: 'auto', label: '跟随系统' },
  { value: 'light', label: '亮色' },
  { value: 'dark', label: '暗色' },
]

const mode = ref('auto')
const open = ref(false)
const root = ref(null)

const readStoredMode = () => {
  try {
    const v = window.localStorage.getItem(STORAGE_KEY)
    return v === 'light' || v === 'dark' ? v : 'auto'
  } catch (e) {
    return 'auto'
  }
}

const writeStoredMode = (next) => {
  mode.value = next
  try {
    window.localStorage.setItem(STORAGE_KEY, next)
    // 主题内部用 vueuse 的 useStorage 持有同键的一份 ref；同一页面里直接写
    // localStorage 不会让那份 ref 同步，必须补一个 storage 事件让它读回。
    // （vueuse 的 useStorage 监听了该事件，且要求 event.storageArea 就是
    //  localStorage —— 见 @vueuse/core 的 useStorage#update。）
    window.dispatchEvent(
      new StorageEvent('storage', {
        key: STORAGE_KEY,
        newValue: next,
        storageArea: window.localStorage,
      }),
    )
  } catch (e) {
    // 隐私模式下 localStorage 可能不可写：外观保持不变，不报错。
  }
}

const currentLabel = computed(() => {
  const hit = MODES.find((m) => m.value === mode.value)
  return hit ? hit.label : MODES[0].label
})

const select = (value) => {
  writeStoredMode(value)
  open.value = false
}

const onDocClick = (e) => {
  if (root.value && !root.value.contains(e.target)) {
    open.value = false
  }
}

const onKeydown = (e) => {
  if (e.key === 'Escape') {
    open.value = false
  }
}

onMounted(() => {
  mode.value = readStoredMode()
  document.addEventListener('click', onDocClick)
  document.addEventListener('keydown', onKeydown)
})

onBeforeUnmount(() => {
  document.removeEventListener('click', onDocClick)
  document.removeEventListener('keydown', onKeydown)
})
</script>

<template>
  <div ref="root" class="tms-mode">
    <button
      type="button"
      class="tms-mode__btn"
      :title="'配色模式：' + currentLabel"
      :aria-label="'配色模式：' + currentLabel"
      aria-haspopup="menu"
      :aria-expanded="open ? 'true' : 'false'"
      @click="open = !open"
    >
      <svg v-if="mode === 'light'" class="tms-mode__icon" viewBox="0 0 24 24" aria-hidden="true">
        <circle cx="12" cy="12" r="4.2" fill="currentColor" />
        <g stroke="currentColor" stroke-width="1.7" stroke-linecap="round" fill="none">
          <path d="M12 2.6v2.3M12 19.1v2.3M2.6 12h2.3M19.1 12h2.3M5.4 5.4l1.7 1.7M16.9 16.9l1.7 1.7M18.6 5.4l-1.7 1.7M7.1 16.9l-1.7 1.7" />
        </g>
      </svg>
      <svg v-else-if="mode === 'dark'" class="tms-mode__icon" viewBox="0 0 24 24" aria-hidden="true">
        <path d="M20.6 14.6A8.7 8.7 0 0 1 9.4 3.4a8.7 8.7 0 1 0 11.2 11.2z" fill="currentColor" />
      </svg>
      <svg v-else class="tms-mode__icon" viewBox="0 0 24 24" aria-hidden="true">
        <circle cx="12" cy="12" r="8.4" fill="none" stroke="currentColor" stroke-width="1.7" />
        <path d="M12 3.6a8.4 8.4 0 0 1 0 16.8z" fill="currentColor" />
      </svg>
    </button>

    <div v-show="open" class="tms-mode__menu" role="menu">
      <button
        v-for="m in MODES"
        :key="m.value"
        type="button"
        class="tms-mode__item"
        role="menuitemradio"
        :aria-checked="mode === m.value ? 'true' : 'false'"
        @click="select(m.value)"
      >
        <span class="tms-mode__label">{{ m.label }}</span>
        <span v-if="mode === m.value" class="tms-mode__check" aria-hidden="true">&#10003;</span>
      </button>
    </div>
  </div>
</template>

<style scoped>
.tms-mode {
  position: relative;
  display: flex;
  margin-inline-start: 1rem;
}

.tms-mode__btn {
  display: flex;
  align-items: center;
  padding: 0;
  border: 0;
  background: none;
  color: var(--vp-c-text);
  opacity: 0.8;
  cursor: pointer;
}

.tms-mode__btn:hover {
  opacity: 1;
}

.tms-mode__icon {
  width: 1.25rem;
  height: 1.25rem;
}

.tms-mode__menu {
  position: absolute;
  top: calc(100% + 8px);
  right: 0;
  z-index: 100;
  min-width: 8.5rem;
  padding: 4px;
  border: 1px solid var(--vp-c-border);
  border-radius: 10px;
  background: var(--vp-c-bg-elv);
  box-shadow: 0 10px 30px var(--vp-c-shadow);
}

.tms-mode__item {
  display: flex;
  align-items: center;
  justify-content: space-between;
  width: 100%;
  padding: 7px 10px;
  border: 0;
  border-radius: 6px;
  background: none;
  color: var(--vp-c-text);
  font-size: 13.5px;
  line-height: 1.4;
  text-align: left;
  cursor: pointer;
}

.tms-mode__item:hover {
  background: var(--vp-c-control-hover);
}

.tms-mode__check {
  color: var(--vp-c-accent);
}

@media print {
  .tms-mode {
    display: none;
  }
}
</style>
