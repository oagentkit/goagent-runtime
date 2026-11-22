<script setup lang="ts">
import { onBeforeUnmount, onMounted, ref } from 'vue'
import mermaid from 'mermaid'

const props = defineProps<{ code: string }>()
const el = ref<HTMLElement | null>(null)

// mermaid.render 不支持并发调用，且各组件实例的渲染 id 必须唯一：
// 全局串行队列 + 随机 id，两者缺一不可（并发/重 id 会让图空白或错位）。
declare global {
  interface Window { __mermaidQueue?: Promise<void> }
}

const isDark = (): boolean =>
  document.documentElement.getAttribute('data-theme') === 'dark'

function enqueue(job: () => Promise<void>): void {
  const prev = window.__mermaidQueue ?? Promise.resolve()
  const next = prev.then(job).catch(() => {})
  window.__mermaidQueue = next
}

async function render(): Promise<void> {
  if (!el.value) return
  const dark = isDark()
  // 主题跟随站点暗/亮模式：暗色用 mermaid 官方 dark 主题，亮色用 neutral（不发黄）。
  mermaid.initialize({
    startOnLoad: false,
    theme: dark ? 'dark' : 'neutral',
    themeVariables: {
      fontFamily: '-apple-system, "PingFang SC", "Hiragino Sans GB", "Microsoft YaHei", sans-serif',
      fontSize: '15px',
    },
    flowchart: { htmlLabels: true, curve: 'basis', padding: 12 },
    sequence: { actorMargin: 56, mirrorActors: false, wrap: true, width: 160 },
  })
  try {
    const id = 'mmd-' + Math.random().toString(36).slice(2, 12)
    const { svg, bindFunctions } = await mermaid.render(id, decodeURIComponent(props.code))
    if (!el.value) return
    el.value.innerHTML = svg
    bindFunctions?.(el.value)
    el.value.classList.toggle('is-dark', dark)
  } catch (e) {
    const msg = e instanceof Error ? e.message : String(e)
    if (el.value) {
      el.value.innerHTML = '<pre style="color:#e5534b;text-align:left;white-space:pre-wrap">mermaid 渲染失败：' +
        msg.replace(/</g, '&lt;') + '</pre>'
    }
  }
}

let observer: MutationObserver | null = null

onMounted(() => {
  enqueue(render)
  // 站点暗/亮切换（html data-theme 变化）时重渲染
  observer = new MutationObserver(() => enqueue(render))
  observer.observe(document.documentElement, { attributes: true, attributeFilter: ['data-theme'] })
})

onBeforeUnmount(() => observer?.disconnect())
</script>

<template>
  <div ref="el" class="mermaid-diagram"></div>
</template>

<style>
.mermaid-diagram {
  margin: 20px 0;
  padding: 18px 14px;
  border-radius: 10px;
  overflow-x: auto;
  text-align: center;
  background: #ffffff;
  border: 1px solid #e5e7eb;
  box-shadow: 0 4px 18px rgba(0, 0, 0, 0.06);
}
.mermaid-diagram.is-dark {
  background: #16181d;
  border-color: #30363d;
  box-shadow: 0 4px 18px rgba(0, 0, 0, 0.35);
}
.mermaid-diagram svg {
  max-width: 100%;
  height: auto;
}
</style>
