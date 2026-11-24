/**
 * mermaid 渲染队列排空信号。
 *
 * 为什么需要这个信号：mermaid 图是**纯客户端**渲染的——围栏在构建期只产出一个
 * 空容器（见 config.ts 的 extendsMarkdown），SVG 要等浏览器里 mermaid.render()
 * 跑完才注入容器 ⇒ 容器高度从「只有 padding/border/margin」长到几百 px。
 *
 * 锚点定位（浏览器自身的 hash 定位、vue-router 的 scrollBehavior）算在
 * **图还没长出来**的那份布局上 ⇒ 图长出来后没人重新定位 ⇒ 目标被推到视口
 * 边缘甚至视口外（实测 recovery 页偏 577px），且冷/热加载时对时错。
 *
 * 于是需要一个「布局已稳定」的信号，让定位方能在稳定后重算一次。
 * 事件名放在独立模块里 ⇒ 派发方与消费方共用同一份字符串，不会各写一份而漂移。
 */
export const MERMAID_IDLE_EVENT = 'mermaid:idle'

/**
 * 当前还有几张 mermaid 图在排队渲染（0 = 队列排空 = 布局已定）。
 *
 * 为什么锚点定位要读它：mermaid 是**纯客户端**渲染的，围栏在构建期只产出空
 * 容器 ⇒ 图一张张长出来 ⇒ 锚点算在「还在长」的布局上。队列排空是「布局已定」
 * 的**权威信号**（比「位置连续几拍没变」这种启发式可靠：组件重挂载会让内容
 * 高度来回变，几拍不变不等于终态）。
 *
 * 没有图的页面恒为 0（__mermaidPending 未定义 ⇒ 0）⇒ 不影响普通页面。
 */
export function mermaidPending(): number {
  return (window as unknown as { __mermaidPending?: number }).__mermaidPending ?? 0
}
