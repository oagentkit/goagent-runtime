<script setup lang="ts">
import { onBeforeUnmount, onMounted, ref } from 'vue'
import mermaid from 'mermaid'
import { MERMAID_IDLE_EVENT } from '../mermaid-events'

const props = defineProps<{ code: string }>()
const el = ref<HTMLElement | null>(null)

// mermaid.render 不支持并发调用，且各组件实例的渲染 id 必须唯一：
// 全局串行队列 + 随机 id，两者缺一不可（并发/重 id 会让图空白或错位）。
declare global {
  interface Window {
    __mermaidQueue?: Promise<void>
    __mermaidPending?: number
  }
}

const isDark = (): boolean =>
  document.documentElement.getAttribute('data-theme') === 'dark'

function enqueue(job: () => Promise<void>): void {
  const prev = window.__mermaidQueue ?? Promise.resolve()
  window.__mermaidPending = (window.__mermaidPending ?? 0) + 1
  const next = prev
    .then(job)
    .catch(() => {})
    .then(() => {
      window.__mermaidPending = (window.__mermaidPending ?? 1) - 1
      // 队列排空 = 布局稳定（所有图的最终高度都已落定）。
      // 派发信号，让锚点定位方有机会重算一次目标位置——
      // 锚点算在「图还没长出来」的布局上，不重定位就会偏（见 mermaid-events.ts）。
      if (window.__mermaidPending <= 0) {
        window.__mermaidPending = 0
        window.dispatchEvent(new CustomEvent(MERMAID_IDLE_EVENT))
      }
    })
  window.__mermaidQueue = next
}

async function render(): Promise<void> {
  if (!el.value) return
  const dark = isDark()
  // 主题跟随站点暗/亮模式：暗色用 mermaid 官方 dark 主题，亮色用 neutral（不发黄）。
  // 自定义 base 主题：节点/连线/字色全部按站点设计语言走，不再用 mermaid 默认配色。
  const fontFamily = '-apple-system, "PingFang SC", "Hiragino Sans GB", "Microsoft YaHei", sans-serif'
  const vars = dark ? {
    primaryColor: '#21262d', primaryTextColor: '#e6edf3', primaryBorderColor: '#58a6ff',
    lineColor: '#7d8590', textColor: '#e6edf3',
    secondaryColor: '#16181d', tertiaryColor: '#16181d',
    clusterBkg: '#1c2128', clusterBorder: '#3d444d', edgeLabelBackground: '#16181d',
    actorBkg: '#21262d', actorBorder: '#58a6ff', actorTextColor: '#e6edf3',
    signalColor: '#7d8590', signalTextColor: '#e6edf3',
    labelBoxBkgColor: '#21262d', labelTextColor: '#e6edf3', loopTextColor: '#9aa7b8',
    noteBkgColor: '#21262d', noteBorderColor: '#3d444d', noteTextColor: '#e6edf3',
    activationBorderColor: '#58a6ff', activationBkgColor: '#21262d',
    sequenceNumberColor: '#58a6ff', mainBkg: '#21262d', nodeBorder: '#58a6ff',
  } : {
    primaryColor: '#f6f8fa', primaryTextColor: '#1f2328', primaryBorderColor: '#0969da',
    lineColor: '#59636e', textColor: '#1f2328',
    secondaryColor: '#ffffff', tertiaryColor: '#ffffff',
    clusterBkg: '#f6f8fa', clusterBorder: '#d1d9e0', edgeLabelBackground: '#ffffff',
    actorBkg: '#f6f8fa', actorBorder: '#0969da', actorTextColor: '#1f2328',
    signalColor: '#59636e', signalTextColor: '#1f2328',
    labelBoxBkgColor: '#f6f8fa', labelTextColor: '#1f2328', loopTextColor: '#59636e',
    noteBkgColor: '#f6f8fa', noteBorderColor: '#d1d9e0', noteTextColor: '#1f2328',
    activationBorderColor: '#0969da', activationBkgColor: '#f6f8fa',
    sequenceNumberColor: '#0969da', mainBkg: '#f6f8fa', nodeBorder: '#0969da',
  }
  mermaid.initialize({
    startOnLoad: false,
    theme: 'base',
    themeVariables: { fontFamily, fontSize: '15px', ...vars },
    flowchart: { htmlLabels: true, curve: 'basis', padding: 12, nodeSpacing: 30, rankSpacing: 42 },
    sequence: { actorMargin: 60, mirrorActors: false, wrap: true, width: 170, boxMargin: 12, messageMargin: 34 },
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
