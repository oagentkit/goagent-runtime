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
 */

/* ── 读者意图 vs 框架派生态：上一版修错的根因面 ──────────────────────────
 *
 * 实测（headless Chrome，**线上** recovery.html，真实计时——不是虚拟时间）：
 *
 *   t=2106  scrollY 0 → 1207      ← 落位**正确**（目标标题在导航栏下方 70px）
 *   t=2347  replaceState("...#恢复时的重放判定")
 *   t=3102  scrollY 1207 → 631    ← 守卫照着改写后的 hash 追 ⇒ 被拽上去
 *   t=3350  replaceState("...#先建立画面-提交点")
 *   t=4102  scrollY 631 → 76      ← 再被拽一次 ⇒ 连跳两次 ⇒ 观感就是「抖」
 *
 * 改写 hash 的是主题自带的 `@vuepress/plugin-active-header-links`：它**随滚动
 * 把「当前所在章节」写进地址栏**（插件实现：监听 scroll → 比对各标题的
 * offsetTop → router.replace({hash: 当前章节})）⇒ **地址栏里的 hash 是框架的
 * 派生态，不是读者的意图**。
 *
 * ⇒ 守卫的目标必须来自「读者明确要的锚点」（URL 初始 hash / 点锚点 / 路由
 *   push / 前进后退），**不能**每拍去读 window.location.hash。框架往地址栏写
 *   什么，跟「读者要去哪」是两件事。
 *
 * 反证（把框架那次改写掐掉，运行时替换 router.replace）：同一页面落在
 * scrollY=1207、标题 top=70、地址栏 hash 不变 ⇒ 抖动只由「追派生态」引起。
 */

/** 读者明确要落的锚点 id（落位目标的唯一真相）。 */
let intentId = ''

/** 路由器实例（enhance 里拿到；点原生锚点后要靠它改地址栏）。 */
let routerRef: Router | null = null

/**
 * 路由的**初始导航**是否已完成。
 *
 * 为什么要有这道闸：初始导航完成前，路由的 currentRoute 还停在 '/' ⇒ 此时对
 * 路由做任何 replace/push 都会把**待执行的初始导航顶掉** ⇒ 整站落到首页
 * （实测：深链进来时地址栏的 hash 被「钉」回去，结果页面被换成首页）。
 * ⇒ 动路由之前必须确认它已经就位。
 */
let routerReady = false

/** 解 hash 字符串（可带 '#'、可百分号编码）⇒ 锚点 id。 */
function decodeHash(raw: string): string {
  const s = raw.startsWith('#') ? raw.slice(1) : raw
  if (!s) return ''
  try {
    return decodeURIComponent(s)
  } catch {
    /* 解不开就按原样用 */
    return s
  }
}

/** 导航目标里的 hash id（字符串或路由对象都吃）。 */
function hashOfTarget(to: unknown): string {
  if (typeof to === 'string') {
    const i = to.indexOf('#')
    return i >= 0 ? decodeHash(to.slice(i)) : ''
  }
  if (to && typeof to === 'object') {
    const h = (to as { hash?: string }).hash
    return h ? decodeHash(h) : ''
  }
  return ''
}

/** 导航目标里的 path（字符串或路由对象都吃；取不到返回空串）。 */
function pathOfTarget(to: unknown): string {
  if (typeof to === 'string') {
    const cut = to.search(/[?#]/)
    return cut >= 0 ? to.slice(0, cut) : to
  }
  if (to && typeof to === 'object') {
    return (to as { path?: string }).path ?? ''
  }
  return ''
}

/** 用户在两次对齐之间是否自己动过页面（滚轮 / 触摸 / 翻页键）。 */
let userScrolled = false

/* ── 滚动接管：框架那条「按陈旧布局算的平滑滚动」必须被改写 ──────────────
 *
 * 框架那条滚动算的是**内容还没长完时**的位置（实测 top=695.9 / 699.8 / 145.6，
 * 正确值是 1207），而且 behavior=undefined ⇒ 页面 `html{scroll-behavior:smooth}`
 * ⇒ 浏览器把它当成**平滑滚动** ⇒ 从旧位置一路滑到新位置 ⇒ 观感也是「抖」。
 *
 * 为什么不在 scrollBehavior 那一层接管就完事：那一层只能让框架**别滚**，
 * 而 `enhance` 钩子里能不能拿到 router 是框架决定的（实测能拿到：VuePress
 * 把 router 作为 enhance 的第二个参数传进来）⇒ 那一层已经接管（见文件末尾），
 * 这里的滚动层接管是**第二道防线**：接管窗口内，任何外来滚动（哪怕绕过
 * scrollBehavior 直接调 scrollTo/scrollIntoView）都被改写成「此刻的正确目标」
 * 并强制 instant。
 */

/** 滚动接管窗口是否开着（开着时，外来的滚动不再落地）。 */
let guardOn = false

/** 此刻算出来的正确纵向目标（接管窗口内，外来的滚动按它改写）。 */
let guardTop = 0

/** 本次滚动是**我们自己**发起的（⇒ 接管窗口不许改写它）。
 *
 * 为什么要有这个旗标：接管窗口会把外来滚动改写成「此刻的正确目标」，而**我们
 * 自己**的滚动已经是按此刻布局算出来的 ⇒ 再被改写一次就等于被拽回旧目标
 * （实测：点「最后一个小节」时，守卫把这次滚动改写回上一个目标 ⇒ 点不动）。
 */
let ownScroll = false

/** 自己滚到某个纵向位置（绕过接管窗口的改写）。 */
function scrollToTop(top: number): void {
  ownScroll = true
  try {
    window.scrollTo({ top, behavior: 'instant' })
  } finally {
    ownScroll = false
  }
}

/** 接管纵向滚动：把「陈旧布局算出来的目标」换成「此刻的正确目标」。 */
function installScrollGuard(): void {
  const nativeScrollTo = window.scrollTo.bind(window)
  const nativeSIV = Element.prototype.scrollIntoView
  window.scrollTo = function (...args: unknown[]): void {
    if (!guardOn || ownScroll) {
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
    if (guardOn && id && id === intentId) {
      nativeSIV.call(this, { block: 'start', behavior: 'instant' })
      return
    }
    nativeSIV.apply(this, args as [boolean | ScrollIntoViewOptions])
  }
}

function markUserScrolled(): void {
  userScrolled = true
}

/**
 * 锚点目标元素（意图指向的元素不在当前页面上 ⇒ null ⇒ 本轮不落位）。
 *
 * 为什么查「意图」而不是查地址栏：见文件头——地址栏里的 hash 是框架随滚动
 * 改写的派生态，照着它走会被拽走（实测连跳两次）。
 */
function hashTarget(): HTMLElement | null {
  if (!intentId) return null
  return document.getElementById(intentId)
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

/** 把目标滚到它该在的位置（数值化：位置由此刻的 rect 算，滚动由自己做）。 */
function scrollTargetTo(el: HTMLElement): void {
  const margin = targetOffset(el)
  // getBoundingClientRect 会强制刷新布局 ⇒ 读到的就是**此刻**的真实位置。
  const rect = el.getBoundingClientRect()
  const top = Math.round(rect.top + window.scrollY - margin)
  // instant：顺带打断正在飞的平滑滚动（新的滚动会取消旧的动画）。
  scrollToTop(top)
}

/** 对齐循环：每隔一段时间复查一次，直到「落定」。 */
const ALIGN_TICK_MS = 250
const ALIGN_MAX_MS = 15000
const ALIGN_STABLE_TICKS = 2 // 目标位置连续几拍不变 ⇒ 布局已定

let alignTimer: number | undefined
let alignDeadline = 0
let alignLastTop = Number.NaN
let alignStable = 0
/** 本轮是否已经落定（落定后**不再**因为读者滚走而把页面拽回去）。 */
let alignSettled = false

function alignTick(): void {
  alignTimer = undefined
  if (userScrolled) {
    guardOn = false
    return // 用户自己动了 ⇒ 收手，别把读者拽回锚点
  }
  // 一拍里抛异常不该**断掉整条链**（下一拍还要复查）：异常只记一笔，链继续。
  try {
    alignOnce()
  } catch (e) {
    console.error('[align] 对齐复查异常（不影响后续复查）:', e)
  }
  if (Date.now() >= alignDeadline) {
    guardOn = false
    return
  }
  alignTimer = window.setTimeout(alignTick, ALIGN_TICK_MS)
}

// alignOnce 一拍复查：算目标 → 布局已定才滚 → 更新接管窗口的目标。
function alignOnce(): void {
  const y = Math.round(window.scrollY)
  const el = hashTarget()
  if (!el) return

  const margin = targetOffset(el)
  const rect = el.getBoundingClientRect()
  // 目标该在的纵向位置（= 此刻的真实布局算出来的那一个）。
  const top = Math.round(rect.top + window.scrollY - margin)
  // 目标够不到顶（文档到底了）⇒ 落到最底。
  //
  // 浏览器自己也会把滚动截断到文档底部，但**判据**必须按截断后的值算：否则
  // 「已在位」的判据永远差那几像素 ⇒ 接管窗口一直开着 ⇒ 读者往上滚会被劫持
  // （页面最后一个小节最容易踩到这条）。
  const maxY = Math.max(0, document.documentElement.scrollHeight - window.innerHeight)
  const wantY = Math.min(top, maxY)

  // ★ 只在**布局已定**时才滚。
  //
  //   两条同时成立才算定：① mermaid 队列排空（没有图还在排队渲染——组件重挂载
  //   会让图被清掉再渲染，高度来回变，几拍不变**不等于**终态）；② 目标连续
  //   几拍算出来一样。
  //
  //   布局没定时滚到中途位置，就是「跳一下、再跳一下」的抖动来源（实测里
  //   框架就滚到了 630 / 76 这两个假位置）。
  // 落位期间把地址栏的 hash **钉在读者要的锚点**上。
  //
  // 为什么：框架的 active-header-links 插件随滚动改写地址栏（落位期间页面还在
  // 顶部 ⇒ 插件按「读者在页首」把 hash 清空）⇒ 深链的锚点在地址栏里短暂消失
  // （实测：加载期间 hash 变 ""，落位后才又回来）⇒ 读者看到地址栏闪一下。
  // 落位之后由插件接管（读者滚动时地址栏跟着章节走，是它的正经用途）。
  if (!alignSettled && intentId && routerRef && routerReady) {
    const want = `#${intentId}`
    if (window.location.hash !== want) {
      routerRef.replace(want).catch(() => {
        /* 重复导航等失败情形不影响落位 */
      })
    }
  }

  const pending = mermaidPending()
  // 记的是 wantY（**够得着**的那个位置），不是 top：目标够不到顶时 top 会随
  // 布局变，而 wantY 才是真正要落到的位置 ⇒ 判据与滚动用同一个数。
  const layoutMoved = wantY !== alignLastTop
  if (layoutMoved) {
    alignStable = 0
    alignLastTop = wantY
  } else {
    alignStable += 1
  }

  if (pending !== 0 || alignStable < ALIGN_STABLE_TICKS) {
    // 布局还没定 ⇒ 压制外来的滚动（保持原位），别让它把我们拽到假位置。
    guardTop = y
    return
  }

  // 布局已定 ⇒ 目标位置可信了。
  guardTop = wantY
  // 目标位置**变了**（布局在长）⇒ 要重新落位；位置没变 ⇒ 要么已经在位，
  // 要么是读者自己滚走了（不再拽回去）。
  if (layoutMoved || !alignSettled) {
    if (Math.abs(y - wantY) > 2) {
      scrollToTop(wantY)
      alignSettled = false
    } else {
      alignSettled = true
      guardOn = false // 已在位 ⇒ 把滚动权交还（页内其它跳转不再被劫持）
    }
  }
}

/** 起一轮对齐复查（已有的那轮会被延长）。 */
function requestAlign(): void {
  alignDeadline = Date.now() + ALIGN_MAX_MS
  alignLastTop = Number.NaN
  alignStable = 0
  alignSettled = false
  // 有锚点目标才开接管窗口（没有意图的页面不劫持任何滚动）。
  if (hashTarget()) guardOn = true
  if (alignTimer !== undefined) return
  alignTimer = window.setTimeout(alignTick, 0)
}

/** 设定「读者要落的锚点」并起一轮对齐（同一锚点重复点击不重置）。 */
function setIntent(id: string): void {
  if (id === intentId) return
  intentId = id
  // 意图变了 ⇒ 接管窗口的目标也要**立刻**换：否则这一拍里进来的外来滚动
  // 会被改写到**旧**目标上（实测：点另一个小节，滚动被拽回上一个）。
  const el = hashTarget()
  if (el) {
    const margin = targetOffset(el)
    const rect = el.getBoundingClientRect()
    guardTop = Math.round(rect.top + window.scrollY - margin)
  }
  requestAlign()
}

/* ⚠ 这个文件在**构建期**也会被 import（VuePress 用它渲染页面）⇒
 * 顶层不能直接摸 window/document，必须判过再用。 */
if (typeof window !== 'undefined' && typeof document !== 'undefined') {
  // 读者带着深链进来 ⇒ 初始意图 = URL 上的 hash（此后框架再改写它也不影响）。
  intentId = decodeHash(window.location.hash)

  // 接管必须**先于**框架第一次 hash 定位装上（模块顶层就在 app 创建之前跑）。
  installScrollGuard()
  window.addEventListener('wheel', markUserScrolled, { passive: true })
  window.addEventListener('touchmove', markUserScrolled, { passive: true })
  window.addEventListener('keydown', (e) => {
    if (['ArrowUp', 'ArrowDown', 'PageUp', 'PageDown', 'Home', 'End', ' '].includes(e.key)) {
      markUserScrolled()
    }
  })

  // 前进后退 ⇒ 意图 = 回到的那个地址的 hash。
  window.addEventListener('hashchange', () => {
    userScrolled = false
    setIntent(decodeHash(window.location.hash))
  })
  window.addEventListener('popstate', () => {
    userScrolled = false
    setIntent(decodeHash(window.location.hash))
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

  // 页内锚点点击：自己滚，不交给浏览器（浏览器按**陈旧布局**平滑滚 ⇒ 抖）。
  document.addEventListener('click', (e) => {
    const target = e.target as HTMLElement | null
    const a = target?.closest?.('a[href]') as HTMLAnchorElement | null
    if (!a) return
    const href = a.getAttribute('href') ?? ''
    if (!href.startsWith('#')) return
    const id = decodeHash(href)
    if (!id) return

    // 路由链接（侧栏目录）：交给 vue-router 走（push ⇒ 意图由 push 钩子接管），
    // 这里只补一次「立刻落位」，不等对齐循环。
    if (!a.classList.contains('route-link')) {
      // 原生锚点（标题旁的 # 链接）：拦下来——浏览器会按陈旧布局做**平滑**滚动。
      e.preventDefault()
      userScrolled = false
      setIntent(id)
      // 地址栏也要跟着改（浏览器原生行为是改 hash + 加一条历史）⇒ 走路由 push。
      if (routerRef) {
        // ★ 必须用**字符串**形式：`router.push({hash})` 会被 vue-router 当成
        //   「相对当前位置的定位」⇒ 现有路径被当成参数值编码（实测
        //   /advanced/recovery.html → /advanced%2Frecovery.html）⇒ 整页重载。
        //   字符串形式与侧栏 RouterLink（href="#id"）走的是同一条解析路径。
        routerRef.push(`#${id}`).catch(() => {
          /* 重复导航等失败情形不影响落位 */
        })
      } else {
        history.replaceState(null, '', `#${encodeURIComponent(id)}`)
      }
    }

    // 同一锚点重复点击：vue-router 判「重复导航」⇒ 不滚 ⇒ 这里自己滚。
    userScrolled = false
    const el = document.getElementById(id)
    if (el) scrollTargetTo(el)
  })
}

export default defineClientConfig({
  // ★ router 由 VuePress 直接传进来（enhance?.({ app, router, siteData })）。
  //   此前从 app.config.globalProperties.$router 取——那个位置在 enhance
  //   执行时**恒为 undefined** ⇒ 接管从未生效 ⇒ 框架照旧按陈旧布局做
  //   **平滑**滚动 ⇒ 抖动。
  enhance({ app, router }) {
    app.component('Mermaid', Mermaid)
    app.component('ArchDiagram', ArchDiagram)

    if (router) {
      routerRef = router
      // 初始导航完成前不许动路由（见 routerReady 的注释）。
      router.isReady().then(
        () => {
          routerReady = true
        },
        () => {
          /* 初始导航失败：保持未就绪 ⇒ 不动路由 */
        }
      )

      // 接管 hash 定位：框架自己的 scrollBehavior 用**陈旧布局**算目标位置。
      //
      // 实测（recovery 页）：框架在内容还没长完时算出的目标是 `top: 76`——
      // 那是元素文档位置还只有 146 时的值；等三张图渲染完，元素真实文档位置
      // 变成 1277，框架那次滚动仍把页面按旧值落下 ⇒ 锚点偏出 577px，而且它
      // 是在**平滑动画**里落地的 ⇒ 观感就是「抖」。
      //
      // 所以这里让框架**不要管 hash 定位**（返回 false ⇒ vue-router 不滚），
      // 全部交给下面的对齐循环：它按**此刻**的真实布局数值化地滚，一步到位。
      // 非 hash 的导航仍走框架原本的行为。
      const prev = router.options.scrollBehavior
      const next: RouterScrollBehavior = (to, from, savedPosition) => {
        if (savedPosition) return savedPosition
        if (to.hash) return false // hash 定位 ⇒ 对齐循环接管
        return prev ? prev(to, from, savedPosition) : { left: 0, top: 0 }
      }
      router.options.scrollBehavior = next
      router.scrollBehavior = next

      // 意图的第一来源：路由 push（同页换锚点、侧栏目录点击）。
      //
      // 为什么只认 push 不认 replace：框架的 active-header-links 插件随滚动
      // 用 replace 把「当前所在章节」写回地址栏 ⇒ replace 是**派生态**的落地
      // 方式，不是读者的意图（见文件头）。
      const rawPush = router.push.bind(router)
      router.push = ((to: unknown) => {
        const id = hashOfTarget(to)
        const curPath = router.currentRoute.value.path
        const nextPath = pathOfTarget(to) || curPath
        if (id) {
          setIntent(id)
        } else if (nextPath !== curPath) {
          // 换页且新页不带 hash ⇒ 旧意图作废（否则同 id 的标题会被误落位）。
          setIntent('')
        }
        return rawPush(to as never)
      }) as typeof router.push
    }
  },
})
