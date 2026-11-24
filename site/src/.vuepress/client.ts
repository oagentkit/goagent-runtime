import { defineClientConfig } from 'vuepress/client'
import type { Router, RouterScrollBehavior } from 'vue-router'
import Mermaid from './components/Mermaid.vue'
import ArchDiagram from './components/ArchDiagram.vue'
import { MERMAID_IDLE_EVENT } from './mermaid-events'

/* ── 锚点落位：内容变高后重新对齐 + 同 hash 点击也滚 ──────────────────────
 *
 * mermaid 图是**纯客户端**渲染的（见 Mermaid.vue 与 mermaid-events.ts）：围栏在
 * 构建期只产出一个空容器，SVG 要等浏览器里 mermaid.render() 跑完才注入 ⇒
 * 容器高度从「只有 padding/border/margin」长到几百 px（本页三张图合计约
 * 1160px）。而锚点定位（浏览器自身的 hash 定位、vue-router 的 scrollBehavior）
 * 算在**内容还没长出来**的那份布局上 ⇒ 落位按旧布局算，与最终位置差出几百 px
 * （实测 recovery 页偏 577px），冷/热加载还时对时错 ⇒ 观感就是「抖」。
 *
 * 为什么是「对齐循环」而不是判一次：浏览器 hash 定位、vue-router 的平滑
 * scrollBehavior、Chrome 的 scroll anchoring 三件事互相抢同一个滚动位置，谁
 * 最后落地取决于时序 ⇒ 判一次必然不稳。改成每隔 250ms 复查一次。
 *
 * 循环什么时候算「稳了」：不是「目标已在位」就算稳——框架那条平滑动画**会从
 * 目标位置旁边滑过去**，那一刻目标看上去正好在位，但动画还没结束 ⇒ 收工就等于
 * 把最后一棒让给了它（实测：循环提前收工 ⇒ 动画随后把页面落在按旧布局算出来的
 * 位置）。真正的稳是：**滚动位置连续 2 秒没动** 且 目标已在位。
 *
 * 为什么不用 scrollIntoView：内容还在变的时候它会用**陈旧的元素位置**算目标
 * （实测：落在按旧布局算出来的位置，差出 1000+ px）。改成**数值化**地滚——位置
 * 由我们自己从**此刻**的 rect 算，滚动由我们自己做。
 *
 * ⚠ 保护只有一条：**用户自己动过页面就不动**（滚轮 / 触摸 / 翻页键）。不能按
 * 「目标在不在视口里」判断——加载途中目标常常还在视口之外，那正是该动的时候。
 */

/** 用户在两次对齐之间是否自己动过页面（滚轮 / 触摸 / 翻页键）。 */
let userScrolled = false

function markUserScrolled(): void {
  userScrolled = true
}

/** 当前 URL 的 hash 目标 id（已解码）。 */
function hashTargetId(): string {
  const raw = window.location.hash
  if (!raw || raw === '#') return ''
  try {
    return decodeURIComponent(raw.slice(1))
  } catch {
    return raw.slice(1)
  }
}

/** 锚点目标元素（没有 hash 或元素不存在 ⇒ null）。 */
function hashTarget(): HTMLElement | null {
  const id = hashTargetId()
  if (!id) return null
  return document.getElementById(id)
}

/**
 * 目标「该在」的视口内位置。
 *
 * 偏移口径只有一份：读目标元素自己的 scroll-margin-top（styles/index.scss 里
 * 定的「导航栏高度 + 余量」）——浏览器与这里的判据共用它，不会各算一套而漂移。
 */
function targetOffset(el: HTMLElement): number {
  return Number.parseFloat(getComputedStyle(el).scrollMarginTop) || 0
}

/** 把目标滚到它该在的位置（数值化，见文件头）。 */
function scrollTargetTo(el: HTMLElement): void {
  const margin = targetOffset(el)
  // getBoundingClientRect 会强制刷新布局 ⇒ 读到的就是**此刻**的真实位置。
  const rect = el.getBoundingClientRect()
  const top = Math.round(rect.top + window.scrollY - margin)
  // instant：顺带打断正在飞的平滑滚动（新的滚动会取消旧的动画）。
  window.scrollTo({ top, behavior: 'instant' })
}

/** 对齐循环：每隔一段时间复查一次，直到「滚动停稳 + 目标在位」。 */
const ALIGN_TICK_MS = 250
const ALIGN_MAX_MS = 15000
const ALIGN_STILL_TICKS = 8 // 连续 2 秒滚动位置没变

let alignTimer: number | undefined
let alignDeadline = 0
let alignStill = 0
let alignLastY = -1

function alignTick(): void {
  alignTimer = undefined
  if (userScrolled) return // 用户自己动了 ⇒ 收手，别把读者拽回锚点

  // 滚动位置有没有在动？
  const y = Math.round(window.scrollY)
  if (y === alignLastY) alignStill += 1
  else {
    alignStill = 0
    alignLastY = y
  }

  const el = hashTarget()
  if (el) {
    const want = targetOffset(el)
    const top = el.getBoundingClientRect().top
    // 目标不在位 ⇒ 拉过去。**不在位就拉**是这里唯一要守的判据：
    // 「已在位就收工」会栽在「内容后来才长高」上——收工那一刻目标确实在位
    // （按旧布局算的），等图渲染完它就被顶走了，却没人再校正。
    if (Math.abs(top - want) > 2) scrollTargetTo(el)
  }

  // 窗口内持续复查（默认 8 秒）：内容什么时候长完是不确定的，
  // 与其猜一个「该稳了」的时机，不如在窗口内一直盯着。
  if (Date.now() >= alignDeadline) return
  alignTimer = window.setTimeout(alignTick, ALIGN_TICK_MS)
}

/** 起一轮对齐复查（已有的那轮会被延长）。 */
function requestAlign(): void {
  alignDeadline = Date.now() + ALIGN_MAX_MS
  alignStill = 0
  if (alignTimer !== undefined) return
  alignTimer = window.setTimeout(alignTick, 0)
}

/* ⚠ 这个文件在**构建期**也会被 import（VuePress 用它渲染页面）⇒
 * 顶层不能直接摸 window/document，必须判过再用。 */
if (typeof window !== 'undefined' && typeof document !== 'undefined') {
  window.addEventListener('wheel', markUserScrolled, { passive: true })
  window.addEventListener('touchmove', markUserScrolled, { passive: true })
  window.addEventListener('keydown', (e) => {
    if (['ArrowUp', 'ArrowDown', 'PageUp', 'PageDown', 'Home', 'End', ' '].includes(e.key)) {
      markUserScrolled()
    }
  })

  // 换锚点 / 前进后退 ⇒ 重新计一次「用户动过」，并起一轮新的对齐。
  window.addEventListener('hashchange', () => {
    userScrolled = false
    requestAlign()
  })
  window.addEventListener('popstate', () => {
    userScrolled = false
    requestAlign()
  })

  // 内容变 ⇒ 目标可能离位 ⇒ 起一轮对齐复查。
  //
  // 盯 DOM 变化（MutationObserver）而不是尺寸：mermaid 的 SVG 注入是「往容器里
  // 塞子节点」，**盒高不变**（容器早就占着 padding+border），ResizeObserver 看
  // 不到；而分批挂载的后续组件也是同一形态。DOM 变了就是内容变了。
  const startObserver = (): void => {
    const root = document.body ?? document.documentElement
    const mo = new MutationObserver(() => requestAlign())
    mo.observe(root, { childList: true, subtree: true, characterData: true })
  }
  if (document.body) startObserver()
  else document.addEventListener('DOMContentLoaded', startObserver, { once: true })

  // mermaid 渲染队列排空 ⇒ 这一批图渲染完了。
  window.addEventListener(MERMAID_IDLE_EVENT, () => requestAlign())

  // 平滑滚动结束 ⇒ 那才是最终位置 ⇒ 再复查一轮。
  window.addEventListener('scrollend', () => requestAlign())

  // 视口尺寸变了 ⇒ 锚点位置会变（导航栏高度、换行宽度）。
  window.addEventListener('resize', () => requestAlign())

  // DOM 就绪后再起首轮（SSR 布局 → 客户端布局的第一次校正）。
  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', () => requestAlign(), { once: true })
  } else {
    requestAlign()
  }

  // 同 hash 锚点点击：vue-router 判「重复导航」⇒ 不滚 ⇒ 这里自己滚。
  document.addEventListener('click', (e) => {
    const target = e.target as HTMLElement | null
    const a = target?.closest?.('a[href]') as HTMLAnchorElement | null
    if (!a) return
    const href = a.getAttribute('href') ?? ''
    if (!href.startsWith('#')) return
    let id = href.slice(1)
    try {
      id = decodeURIComponent(id)
    } catch {
      /* 解不开就按原样用 */
    }
    if (!id || id !== hashTargetId()) return // 不同 hash ⇒ 交给 vue-router
    e.preventDefault()
    const el = document.getElementById(id)
    if (el) scrollTargetTo(el)
  })
}

export default defineClientConfig({
  enhance({ app }) {
    app.component('Mermaid', Mermaid)
    app.component('ArchDiagram', ArchDiagram)

    // 接管 hash 定位：框架自己的 scrollBehavior 用**陈旧布局**算目标位置。
    //
    // 实测（recovery 页）：框架在内容还没长完时算出的目标是 `top: 76`——
    // 那是元素文档位置还只有 146 时的值；等三张图渲染完，元素真实文档位置
    // 变成 1277，框架那次滚动仍把页面按旧值落下 ⇒ 锚点偏出 577px，而且它
    // 是在**平滑动画**里落地的 ⇒ 观感就是「抖」。
    //
    // 所以这里让框架**不要管 hash 定位**（返回 false ⇒ vue-router 不滚），
    // 全部交给上面的对齐循环：它按**此刻**的真实布局数值化地滚，一步到位。
    // 非 hash 的导航仍走框架原本的行为。
    const router = (
      app.config.globalProperties as unknown as { $router?: Router }
    ).$router
    if (router) {
      const prev = router.options.scrollBehavior
      const next: RouterScrollBehavior = (to, from, savedPosition) => {
        if (savedPosition) return savedPosition
        if (to.hash) return false // hash 定位 ⇒ 对齐循环接管
        return prev ? prev(to, from, savedPosition) : { left: 0, top: 0 }
      }
      router.options.scrollBehavior = next
      router.scrollBehavior = next
    }
  },
})
