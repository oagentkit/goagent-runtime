import { defineClientConfig } from 'vuepress/client'
import type { Router, RouterScrollBehavior } from 'vue-router'
import Mermaid from './components/Mermaid.vue'
import ArchDiagram from './components/ArchDiagram.vue'
import { MERMAID_IDLE_EVENT, mermaidPending } from './mermaid-events'

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

/* ── 滚动接管：框架那条「按陈旧布局算的平滑滚动」必须被改写 ──────────────
 *
 * 实测（headless Chrome，recovery 页，滚动调用逐条抓拍）：
 *
 *   [ENV]   scroll-behavior: smooth
 *   [SCROLL] t=539  scrollTo top=695.9375 left=430 behavior=undefined
 *   [SCROLL] t=925  scrollTo top=699.84375 left=430 behavior=undefined
 *   [SCROLL] t=1225 scrollTo top=145.59375 left=430 behavior=undefined
 *
 * 三个 top 分别是**三份陈旧布局**算出来的位置，而正确位置是 1197（元素文档
 * 位置 1267 − scroll-margin-top 70）。而且 behavior=undefined ⇒ 页面
 * `html{scroll-behavior:smooth}` ⇒ 浏览器把它当成**平滑滚动** ⇒ 从旧位置
 * 一路滑到新位置 ⇒ 观感就是「抖」。
 *
 * 为什么不在 scrollBehavior 那一层接管：enhance 钩子里拿到的 router 可能
 * 拿不到（undefined ⇒ 框架那条 scrollBehavior 原样生效 ⇒ 框架照滚不误）。
 * 只能在**滚动这一层**接管：窗口期内把纵向目标改写成「此刻算出来的正确
 * 目标」，并强制 instant（不平滑）——框架那条陈旧滚动就再也落不了地。
 */

/** 滚动接管窗口是否开着（开着时，框架那条陈旧滚动不再落地）。 */
let guardOn = false

/** 此刻算出来的正确纵向目标（接管窗口内，框架的滚动按它改写）。 */
let guardTop = 0

/** 接管纵向滚动：把「陈旧布局算出来的目标」换成「此刻的正确目标」。 */
function installScrollGuard(): void {
  const nativeScrollTo = window.scrollTo.bind(window)
  const nativeSIV = Element.prototype.scrollIntoView
  window.scrollTo = function (...args: unknown[]): void {
    if (!guardOn) {
      nativeScrollTo(...(args as [number, number]))
      return
    }
    const a = args[0] as { top?: number; left?: number } | undefined
    if (a && typeof a === 'object' && typeof a.top === 'number') {
      // 横向照旧（框架那条 left 是把锚点横向对齐），纵向换成此刻的正确目标，
      // 并强制 instant —— 平滑动画是「抖」的另一半成因。
      nativeScrollTo({ ...a, top: guardTop, behavior: 'instant' })
      return
    }
    nativeScrollTo(...(args as [number, number]))
  }
  Element.prototype.scrollIntoView = function (...args: unknown[]): void {
    const id = this.id
    if (guardOn && id && id === hashTargetId()) {
      nativeSIV.call(this, { block: 'start', behavior: 'instant' })
      return
    }
    nativeSIV.apply(this, args as [boolean | ScrollIntoViewOptions])
  }
}

function markUserScrolled(): void {
  userScrolled = true
}

/** 上一次的锚点 id（hash 被路由改写掉时的兜底）。 */
let lastHashId = ''

/**
 * 当前 URL 的 hash 目标 id（已解码）。
 *
 * 为什么留兜底：路由初始化时可能把 URL 上的 hash 改写掉（实测里循环随后就
 * 找不到目标、整条对齐链断掉）——但**锚点元素还在页面上**，读者要的仍然是
 * 那个位置 ⇒ 认最后一次见到的 hash。元素不在了（换页了）⇒ 兜底自然失效
 * （查不到元素就不滚）。
 */
function hashTargetId(): string {
  const raw = window.location.hash
  if (raw && raw !== '#') {
    try {
      lastHashId = decodeURIComponent(raw.slice(1))
    } catch {
      lastHashId = raw.slice(1)
    }
    return lastHashId
  }
  return lastHashId
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
// alignLastTop / alignStable：目标位置连续几次没变才算「布局稳了」。
let alignLastTop = Number.NaN
let alignStable = 0

function alignTick(): void {
  alignTimer = undefined
  if (userScrolled) return // 用户自己动了 ⇒ 收手，别把读者拽回锚点
  // 一拍里抛异常不该**断掉整条链**（下一拍还要复查）：异常只记一笔，链继续。
  try {
    alignOnce()
  } catch (e) {
    console.error('[align] 对齐复查异常（不影响后续复查）:', e)
  }
  if (Date.now() >= alignDeadline) return
  alignTimer = window.setTimeout(alignTick, ALIGN_TICK_MS)
}

// alignOnce 一拍复查：算目标 → 布局已定才滚 → 更新接管窗口的目标。
function alignOnce(): void {

  // 滚动位置有没有在动？
  const y = Math.round(window.scrollY)
  if (y === alignLastY) alignStill += 1
  else {
    alignStill = 0
    alignLastY = y
  }

  const el = hashTarget()
  if (el) {
    const margin = targetOffset(el)
    const rect = el.getBoundingClientRect()
    // 目标该在的纵向位置（= 此刻的真实布局算出来的那一个）。
    const top = Math.round(rect.top + window.scrollY - margin)
    guardTop = top
    // ★ 只在**布局已定**时才滚。
    //
    //   两条同时成立才算定：① mermaid 队列排空（没有图还在排队渲染——组件重挂载
    //   会让图被清掉再渲染，高度来回变，几拍不变**不等于**终态）；② 目标连续
    //   几拍算出来一样。
    //
    //   布局没定时滚到中途位置，就是「跳一下、再跳一下」的抖动来源（实测里
    //   框架就滚到了 630 / 76 这两个假位置）。
    const pending = mermaidPending()
    if (top === alignLastTop) {
      alignStable += 1
    } else {
      alignStable = 0
      alignLastTop = top
    }
    if (pending === 0 && alignStable >= 2) {
      // 目标已确认（终态）⇒ 接管窗口内的外来滚动都按它走。
      guardTop = top
      if (Math.abs(y - top) > 2) {
        window.scrollTo({ top, behavior: 'instant' })
      } else {
        guardOn = false // 已在位 ⇒ 把滚动权交还（页内其它跳转不再被劫持）
      }
    } else {
      // 布局还没定 ⇒ 压制外来的滚动（保持原位），别让它把我们拽到假位置。
      guardTop = y
    }
  }
}

/** 起一轮对齐复查（已有的那轮会被延长）。 */
function requestAlign(): void {
  alignDeadline = Date.now() + ALIGN_MAX_MS
  alignStill = 0
  alignLastTop = Number.NaN
  alignStable = 0
  // 有锚点目标才开接管窗口（没有 hash 的页面不劫持任何滚动）。
  if (hashTarget()) guardOn = true
  if (alignTimer !== undefined) return
  alignTimer = window.setTimeout(alignTick, 0)
}

/* ⚠ 这个文件在**构建期**也会被 import（VuePress 用它渲染页面）⇒
 * 顶层不能直接摸 window/document，必须判过再用。 */
if (typeof window !== 'undefined' && typeof document !== 'undefined') {
  // 接管必须**先于**框架第一次 hash 定位装上（模块顶层就在 app 创建之前跑）。
  installScrollGuard()
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
  // ★ router 由 VuePress 直接传进来（enhance?.({ app, router, siteData })）。
  //   此前从 app.config.globalProperties.$router 取——那个位置在 enhance
  //   执行时**恒为 undefined** ⇒ 接管从未生效 ⇒ 框架照旧按陈旧布局做
  //   **平滑**滚动（实测 top=695.9 / 699.8 / 145.6，正确值 1197）⇒ 抖动。
  enhance({ app, router }) {
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
