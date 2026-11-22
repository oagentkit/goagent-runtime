<script setup lang="ts">
import { onMounted, ref } from 'vue'
import mermaid from 'mermaid'

const props = defineProps<{ code: string }>()
const el = ref<HTMLElement | null>(null)

// mermaid.render 不支持并发调用，且各组件实例的渲染 id 必须唯一：
// 全局串行队列 + 随机 id，两者缺一不可（并发/重 id 会让图空白或错位）。
declare global {
  interface Window { __mermaidQueue?: Promise<void> }
}

onMounted(() => {
  mermaid.initialize({
    startOnLoad: false,
    theme: 'default',
    flowchart: { htmlLabels: true, curve: 'basis' },
    sequence: { actorMargin: 56, mirrorActors: false, wrap: true, width: 160 },
  })
  const prev = window.__mermaidQueue ?? Promise.resolve()
  const job = prev.then(async () => {
    try {
      const id = 'mmd-' + Math.random().toString(36).slice(2, 12)
      const { svg, bindFunctions } = await mermaid.render(id, decodeURIComponent(props.code))
      if (el.value) {
        el.value.innerHTML = svg
        bindFunctions?.(el.value)
      }
    } catch (e) {
      const msg = e instanceof Error ? e.message : String(e)
      if (el.value) {
        el.value.innerHTML = '<pre style="color:#e5534b;text-align:left;white-space:pre-wrap">mermaid 渲染失败：' +
          msg.replace(/</g, '&lt;') + '</pre>'
      }
    }
  })
  window.__mermaidQueue = job
})
</script>

<template>
  <div ref="el" class="mermaid-diagram" style="text-align: center; overflow-x: auto; margin: 16px 0"></div>
</template>
