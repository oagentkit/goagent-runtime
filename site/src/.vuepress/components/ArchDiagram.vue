<template>
  <div class="arch-diagram-wrap">
    <div class="arch-diagram" role="img" aria-label="goagent-runtime 分层架构图：宿主产品 / goagent-runtime / goagent-kernel / 上游原语">
      <svg viewBox="0 0 960 678" xmlns="http://www.w3.org/2000/svg">
        <defs>
          <marker id="ad-arrow" markerWidth="8" markerHeight="8" refX="7" refY="4" orient="auto">
            <path d="M0,0 L8,4 L0,8 z" class="ad-arrow-head" />
          </marker>
        </defs>

        <!-- ── 层带 1：宿主产品 ─────────────────────────────── -->
        <g class="ad-band">
          <rect class="ad-panel" x="16" y="8" width="928" height="104" rx="12" />
          <line class="ad-divider" x1="172" y1="24" x2="172" y2="96" />
          <text class="ad-band-title ad-host" x="30" y="48">宿主产品</text>
          <text class="ad-band-sub" x="30" y="68">你写的领域代码</text>

          <g class="ad-card ad-ext">
            <rect x="186" y="28" width="180" height="64" rx="10" />
            <text class="ad-card-title" x="276" y="56">系统提示词</text>
            <text class="ad-card-sub" x="276" y="76">配置面 · runtime.Config</text>
          </g>
          <g class="ad-card ad-ext">
            <rect x="378" y="28" width="180" height="64" rx="10" />
            <text class="ad-card-title" x="468" y="56">工具 Handler</text>
            <text class="ad-card-sub" x="468" y="76">宿主执行工具</text>
          </g>
          <g class="ad-card ad-ext">
            <rect x="570" y="28" width="180" height="64" rx="10" />
            <text class="ad-card-title" x="660" y="56">模型适配 chat.Model</text>
            <text class="ad-card-sub" x="660" y="76">你的 SDK 适配</text>
          </g>
          <g class="ad-card ad-ext">
            <rect x="762" y="28" width="180" height="64" rx="10" />
            <text class="ad-card-title" x="852" y="56">事件 Sink</text>
            <text class="ad-card-sub" x="852" y="76">日志 · 指标 · 审计</text>
          </g>
        </g>

        <!-- 层间流向 1 -->
        <g class="ad-flow">
          <line x1="192" y1="118" x2="192" y2="152" marker-end="url(#ad-arrow)" />
          <text class="ad-flow-label" x="212" y="140">
            宿主只经三个面接进来：模型 chat.Model · 工具 Handler · 事件 Sink
          </text>
        </g>

        <!-- ── 层带 2：goagent-runtime（本仓）───────────────── -->
        <g class="ad-band">
          <rect class="ad-panel" x="16" y="156" width="928" height="208" rx="12" />
          <line class="ad-divider" x1="172" y1="172" x2="172" y2="348" />
          <text class="ad-band-title ad-runtime" x="30" y="196">goagent-runtime</text>
          <text class="ad-band-sub" x="30" y="216">本仓：装配与默认口径</text>

          <g class="ad-card">
            <rect x="186" y="180" width="236" height="72" rx="10" />
            <text class="ad-card-title" x="304" y="210">模板装配</text>
            <text class="ad-card-sub" x="304" y="230">NewEmbedded · NewStandalone</text>
          </g>
          <g class="ad-card ad-accent">
            <rect x="446" y="180" width="236" height="72" rx="10" />
            <text class="ad-card-title" x="564" y="210">agentloop：可恢复回合循环</text>
            <text class="ad-card-sub" x="564" y="230">预算守卫 + 检查点 + 事件 Emitter</text>
          </g>
          <g class="ad-card">
            <rect x="706" y="180" width="236" height="72" rx="10" />
            <text class="ad-card-title" x="824" y="210">pipeline：工具副作用施加</text>
            <text class="ad-card-sub" x="824" y="230">幂等键 · 资源 · 重试安全</text>
          </g>
          <g class="ad-card">
            <rect x="186" y="268" width="236" height="72" rx="10" />
            <text class="ad-card-title" x="304" y="298">chat：供应商中立窄接口</text>
            <text class="ad-card-sub" x="304" y="318">任何模型 SDK 包一层适配</text>
          </g>
          <g class="ad-card">
            <rect x="446" y="268" width="236" height="72" rx="10" />
            <text class="ad-card-title" x="564" y="298">observe：eventbus → 宿主</text>
            <text class="ad-card-sub" x="564" y="318">事件回调薄装配</text>
          </g>
          <g class="ad-card">
            <rect x="706" y="268" width="236" height="72" rx="10" />
            <text class="ad-card-title" x="824" y="298">storage：11 面存储门面</text>
            <text class="ad-card-sub" x="824" y="318">内存 · sqliteadapter · pgstore</text>
          </g>

          <!-- 装配即接线：模板装配 → agentloop -->
          <line class="ad-edge" x1="424" y1="216" x2="444" y2="216" marker-end="url(#ad-arrow)" />
        </g>

        <!-- 层间流向 2 -->
        <g class="ad-flow">
          <line x1="192" y1="370" x2="192" y2="404" marker-end="url(#ad-arrow)" />
          <text class="ad-flow-label" x="212" y="392">
            runtime 只依赖 kernel 的契约面：接口与判据，不依赖任何宿主实现
          </text>
        </g>

        <!-- ── 层带 3：goagent-kernel（判据）────────────────── -->
        <g class="ad-band">
          <rect class="ad-panel" x="16" y="408" width="928" height="112" rx="12" />
          <line class="ad-divider" x1="172" y1="424" x2="172" y2="504" />
          <text class="ad-band-title ad-kernel" x="30" y="448">goagent-kernel</text>
          <text class="ad-band-sub" x="30" y="468">判据：状态 · 幂等 · 预算 · 恢复</text>

          <g class="ad-card">
            <rect x="186" y="432" width="180" height="64" rx="10" />
            <text class="ad-card-title" x="276" y="460">sideeffect</text>
            <text class="ad-card-sub" x="276" y="480">副作用闸 · 幂等</text>
          </g>
          <g class="ad-card">
            <rect x="378" y="432" width="180" height="64" rx="10" />
            <text class="ad-card-title" x="468" y="460">roundguard</text>
            <text class="ad-card-sub" x="468" y="480">预算回合守卫</text>
          </g>
          <g class="ad-card">
            <rect x="570" y="432" width="180" height="64" rx="10" />
            <text class="ad-card-title" x="660" y="460">eventbus</text>
            <text class="ad-card-sub" x="660" y="480">事件广播判据</text>
          </g>
          <g class="ad-card">
            <rect x="762" y="432" width="180" height="64" rx="10" />
            <text class="ad-card-title" x="852" y="460">state machine</text>
            <text class="ad-card-sub" x="852" y="480">reconcile · trajectory</text>
          </g>
        </g>

        <!-- 层间流向 3 -->
        <g class="ad-flow">
          <line x1="192" y1="526" x2="192" y2="560" marker-end="url(#ad-arrow)" />
          <text class="ad-flow-label" x="212" y="548">
            上游只提供原语：工具治理 · 事件原语 · 并发与日志 · 基础库
          </text>
        </g>

        <!-- ── 层带 4：上游原语 ────────────────────────────── -->
        <g class="ad-band">
          <rect class="ad-panel" x="16" y="564" width="928" height="104" rx="12" />
          <line class="ad-divider" x1="172" y1="580" x2="172" y2="652" />
          <text class="ad-band-title ad-upstream" x="30" y="604">oagentkit / oekit</text>
          <text class="ad-band-sub" x="30" y="624">上游原语</text>

          <g class="ad-card">
            <rect x="186" y="584" width="369" height="64" rx="10" />
            <text class="ad-card-title" x="370" y="612">oagentkit：工具治理 · 派发拓扑</text>
            <text class="ad-card-sub" x="370" y="632">guardx · toolgate · dispatch</text>
          </g>
          <g class="ad-card">
            <rect x="573" y="584" width="369" height="64" rx="10" />
            <text class="ad-card-title" x="757" y="612">oekit：基础库与原语</text>
            <text class="ad-card-sub" x="757" y="632">pkg · concurrency · logx</text>
          </g>
        </g>
      </svg>
    </div>
    <p class="arch-cap">
      四层各司其职：宿主写领域代码，runtime 只做接线与默认口径，kernel 只判定，上游只给原语——箭头只有向下一种方向，依赖从不回流。
    </p>
    <p class="arch-scroll-hint">左右滑动，查看完整架构图 →</p>
  </div>
</template>

<style>
/* ── 视觉口径：与 goagent-kernel 站点同一套（深色 GitHub 风）─────────────
 * 取色板逐值来自 kernel 的 site/docs/.vuepress/styles/index.scss：
 * #0d1117 底 / #11161f 层带 / #161b26 卡 / #262e3d 描边 / #58a6ff 蓝 /
 * #a371f7 紫 / #e6edf3 字 / #9aa7b8 次级字。
 *
 * 为什么自己带一套变量而不直接用 --vp-c-*：runtime 站跑的是 VuePress 默认主题，
 * 它的 --vp-c-* 是默认调色板（绿品牌色），与 kernel 站那套自定义深色不是同一份
 * 真相；直接引用会把图染成默认主题的绿。暗色模式逐值对齐 kernel，亮色模式给同构
 * 的一套浅色（GitHub 浅色风），保证两种模式下图的层级关系一致。
 *
 * 变量挂在 wrap 上而不是 .arch-diagram 上：图注（.arch-cap）是图容器的兄弟节点，
 * 挂在容器上它拿不到变量 → 颜色会掉回继承值（暗色下几乎看不见）。
 */
.arch-diagram-wrap {
  --ad-bg: #ffffff;
  --ad-panel: #f6f8fa;
  --ad-card: #ffffff;
  --ad-card-2: #f6f8fa;
  --ad-border: #d0d7de;
  --ad-border-soft: #e4e8ed;
  --ad-text: #1f2328;
  --ad-mute: #59636e;
  --ad-subtle: #818b98;
  --ad-accent: #0969da;
  --ad-purple: #8250df;
}
html[data-theme='dark'] .arch-diagram-wrap {
  --ad-bg: #0d1117;
  --ad-panel: #11161f;
  --ad-card: #161b26;
  --ad-card-2: #1c2330;
  --ad-border: #262e3d;
  --ad-border-soft: #1f2634;
  --ad-text: #e6edf3;
  --ad-mute: #9aa7b8;
  --ad-subtle: #6b7787;
  --ad-accent: #58a6ff;
  --ad-purple: #a371f7;
}
.arch-diagram {
  margin: 22px 0 6px;
  padding: 16px 10px;
  border-radius: 12px;
  overflow-x: auto;
  /* 不用 text-align: center —— 溢出时居中会让左侧内容永远滚不到（经典居中溢出陷阱） */
  overscroll-behavior-x: contain;
  background: var(--ad-bg);
  border: 1px solid var(--ad-border);
  box-shadow: 0 4px 18px rgba(0, 0, 0, 0.07);
}
html[data-theme='dark'] .arch-diagram {
  box-shadow: 0 6px 24px rgba(0, 0, 0, 0.35);
}
.arch-diagram svg {
  display: block;
  /* 放得下时居中；放不下（min-width 生效）时 margin auto 归零 ⇒ 左对齐、两端可滚 */
  margin: 0 auto;
  max-width: 100%;
  height: auto;
  min-width: 720px;
  font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', 'PingFang SC',
    'Hiragino Sans GB', 'Microsoft YaHei', sans-serif;
}
.arch-diagram .ad-panel {
  fill: var(--ad-panel);
  stroke: var(--ad-border-soft);
  stroke-width: 1;
}
.arch-diagram .ad-divider {
  stroke: var(--ad-border-soft);
  stroke-width: 1;
}
.arch-diagram .ad-band-title {
  font-size: 14px;
  font-weight: 700;
  letter-spacing: 0.2px;
}
.arch-diagram .ad-band-title.ad-host {
  fill: var(--ad-mute);
}
.arch-diagram .ad-band-title.ad-runtime {
  fill: var(--ad-accent);
}
.arch-diagram .ad-band-title.ad-kernel {
  fill: var(--ad-purple);
}
.arch-diagram .ad-band-title.ad-upstream {
  fill: var(--ad-subtle);
}
.arch-diagram .ad-band-sub {
  fill: var(--ad-mute);
  font-size: 11px;
}
.arch-diagram .ad-card rect {
  fill: var(--ad-card);
  stroke: var(--ad-border);
  stroke-width: 1.2;
}
/* 宿主侧的卡：比本仓的卡「更外一层」（kernel 站同款外框层级） */
.arch-diagram .ad-ext rect {
  fill: var(--ad-card-2);
}
.arch-diagram .ad-accent rect {
  stroke: var(--ad-accent);
  stroke-width: 1.8;
}
.arch-diagram .ad-card-title {
  fill: var(--ad-text);
  font-size: 13.5px;
  font-weight: 600;
  text-anchor: middle;
}
.arch-diagram .ad-card-sub {
  fill: var(--ad-mute);
  font-size: 10.5px;
  text-anchor: middle;
}
.arch-diagram .ad-flow line,
.arch-diagram .ad-edge {
  stroke: var(--ad-accent);
  stroke-width: 1.5;
  fill: none;
}
.arch-diagram .ad-arrow-head {
  fill: var(--ad-accent);
}
.arch-diagram .ad-flow-label {
  fill: var(--ad-mute);
  font-size: 11.5px;
}
.arch-cap {
  text-align: center;
  font-size: 12.5px;
  color: var(--ad-mute);
  margin: 6px 0 22px;
}
.arch-scroll-hint {
  display: none;
}
/* 手机断点：内容列最宽 671px < 900px ⇒ 必然可滑，提示不会说谎 */
@media (max-width: 719px) {
  .arch-diagram svg {
    min-width: 900px;
  }
  .arch-scroll-hint {
    display: block;
    margin: -14px 0 0;
    font-size: 12.5px;
    text-align: center;
    color: var(--vp-c-text-mute);
  }
}
</style>
