<template>
  <div class="arch-diagram-wrap">
    <div class="arch-diagram" role="img" aria-label="goagent-runtime 架构图">
      <svg viewBox="0 0 980 640" xmlns="http://www.w3.org/2000/svg">
      <defs>
        <marker id="ad-arrow" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="7" markerHeight="7" orient="auto-start-reverse">
          <path d="M0 0 L10 5 L0 10 z" fill="var(--ad-line)" />
        </marker>
        <filter id="ad-glow" x="-40%" y="-40%" width="180%" height="180%">
          <feDropShadow dx="0" dy="0" stdDeviation="7" flood-color="#58a6ff" flood-opacity="0.45" />
        </filter>
      </defs>

      <!-- 层带 -->
      <rect x="20" y="10" width="940" height="116" rx="12" class="band" style="stroke:#3fb950" />
      <rect x="20" y="142" width="940" height="292" rx="12" class="band" style="stroke:#58a6ff" />
      <rect x="20" y="450" width="604" height="120" rx="12" class="band" style="stroke:#a371f7" />
      <rect x="640" y="450" width="320" height="120" rx="12" class="band" style="stroke:#8b949e" />

      <!-- 层带标题 -->
      <text x="36" y="34" class="band-title" style="fill:#3fb950">宿主产品 —— 你写的领域代码</text>
      <text x="36" y="166" class="band-title" style="fill:#58a6ff">goagent-runtime —— 本仓：装配与默认口径</text>
      <text x="36" y="474" class="band-title" style="fill:#a371f7">goagent-kernel —— 判据（状态/幂等/预算/恢复）</text>
      <text x="656" y="474" class="band-title" style="fill:#8b949e">oagentkit / oekit —— 上游原语</text>

      <!-- 宿主节点 -->
      <g class="node" style="stroke:#3fb950">
        <rect x="36" y="52" width="215" height="56" rx="9" />
        <text x="143" y="85" class="node-text">系统提示词</text>
      </g>
      <g class="node" style="stroke:#3fb950">
        <rect x="267" y="52" width="215" height="56" rx="9" />
        <text x="374" y="85" class="node-text">工具 Handler</text>
      </g>
      <g class="node" style="stroke:#3fb950">
        <rect x="498" y="52" width="215" height="56" rx="9" />
        <text x="605" y="85" class="node-text">模型适配 chat.Model</text>
      </g>
      <g class="node" style="stroke:#3fb950">
        <rect x="729" y="52" width="215" height="56" rx="9" />
        <text x="836" y="77" class="node-text">事件 Sink</text>
        <text x="836" y="95" class="node-sub">日志 / 指标 / 审计</text>
      </g>

      <!-- runtime 节点 -->
      <g class="node" style="stroke:#58a6ff">
        <rect x="368" y="182" width="244" height="54" rx="9" />
        <text x="490" y="206" class="node-text">runtime：模板装配</text>
        <text x="490" y="224" class="node-sub">NewEmbedded · NewStandalone</text>
      </g>
      <g class="node node-accent" style="stroke:#58a6ff" filter="url(#ad-glow)">
        <rect x="328" y="262" width="324" height="70" rx="11" />
        <text x="490" y="292" class="node-text" style="font-size:16px;font-weight:700">agentloop：可恢复回合循环</text>
        <text x="490" y="313" class="node-sub">预算守卫 + 检查点 + 事件 Emitter</text>
      </g>
      <g class="node small" style="stroke:#58a6ff">
        <rect x="36" y="356" width="215" height="56" rx="9" />
        <text x="143" y="380" class="node-text">observe：eventbus → 宿主</text>
        <text x="143" y="398" class="node-sub">事件回调薄装配</text>
      </g>
      <g class="node small" style="stroke:#58a6ff">
        <rect x="267" y="356" width="215" height="56" rx="9" />
        <text x="374" y="380" class="node-text">pipeline：工具副作用施加</text>
        <text x="374" y="398" class="node-sub">幂等键 · 资源 · 重试安全</text>
      </g>
      <g class="node small" style="stroke:#58a6ff">
        <rect x="498" y="356" width="215" height="56" rx="9" />
        <text x="605" y="380" class="node-text">chat：供应商中立窄接口</text>
        <text x="605" y="398" class="node-sub">任何模型 SDK 包一层适配</text>
      </g>
      <g class="node small" style="stroke:#58a6ff">
        <rect x="729" y="356" width="215" height="56" rx="9" />
        <text x="836" y="380" class="node-text">storage：11 面存储门面</text>
        <text x="836" y="398" class="node-sub">内存 · sqliteadapter · pgstore</text>
      </g>

      <!-- kernel 节点 -->
      <g class="node" style="stroke:#a371f7">
        <rect x="36" y="486" width="136" height="58" rx="9" />
        <text x="104" y="510" class="node-text" style="font-size:13px">sideeffect</text>
        <text x="104" y="528" class="node-sub">副作用闸 · 幂等</text>
      </g>
      <g class="node" style="stroke:#a371f7">
        <rect x="182" y="486" width="136" height="58" rx="9" />
        <text x="250" y="510" class="node-text" style="font-size:13px">roundguard</text>
        <text x="250" y="528" class="node-sub">预算回合守卫</text>
      </g>
      <g class="node" style="stroke:#a371f7">
        <rect x="328" y="486" width="136" height="58" rx="9" />
        <text x="396" y="510" class="node-text" style="font-size:13px">eventbus</text>
        <text x="396" y="528" class="node-sub">事件广播判据</text>
      </g>
      <g class="node" style="stroke:#a371f7">
        <rect x="474" y="486" width="136" height="58" rx="9" />
        <text x="542" y="510" class="node-text" style="font-size:13px">state machine</text>
        <text x="542" y="528" class="node-sub">reconcile · trajectory</text>
      </g>

      <!-- 上游节点 -->
      <g class="node" style="stroke:#8b949e">
        <rect x="656" y="486" width="288" height="58" rx="9" />
        <text x="800" y="510" class="node-text" style="font-size:13px">工具治理 · 事件原语</text>
        <text x="800" y="528" class="node-sub">并发与日志 · 基础库</text>
      </g>

      <!-- 连线 -->
      <g class="edge">
        <path d="M143 108 C143 142, 380 150, 425 182" />
        <path d="M374 108 C374 200, 374 300, 374 356" />
        <path d="M605 108 C660 130, 705 210, 655 356" />
        <path d="M143 356 C143 250, 836 250, 836 108" />
        <path d="M490 236 L490 262" />
        <path d="M328 300 C258 300, 200 322, 158 356" />
        <path d="M432 332 C424 344, 404 350, 386 356" />
        <path d="M556 332 C566 344, 588 350, 602 356" />
        <path d="M652 296 C726 298, 800 322, 836 356" />
        <path d="M320 412 C300 452, 200 458, 128 486" />
        <path d="M830 412 C800 452, 570 458, 548 486" />
        <path d="M624 510 L654 510" />
      </g>
      </svg>
    </div>
    <p class="arch-scroll-hint">左右滑动，查看完整架构图 →</p>
  </div>
</template>

<style>
html[data-theme='dark'] .arch-diagram,
html:not([data-theme]) .arch-diagram {
  --ad-text: #e6edf3;
  --ad-text2: #9aa7b8;
  --ad-line: #7d8590;
  --ad-card: #16181d;
  --ad-card-border: #30363d;
}
html[data-theme='light'] .arch-diagram {
  --ad-text: #1f2328;
  --ad-text2: #59636e;
  --ad-line: #59636e;
  --ad-card: #ffffff;
  --ad-card-border: #d1d9e0;
}
.arch-diagram {
  margin: 20px 0;
  padding: 18px 14px;
  border-radius: 12px;
  overflow-x: auto;
  /* 不用 text-align: center —— 溢出时居中会让左侧内容永远滚不到（经典居中溢出陷阱） */
  overscroll-behavior-x: contain;
  background: var(--ad-card);
  border: 1px solid var(--ad-card-border);
  box-shadow: 0 6px 24px rgba(0, 0, 0, 0.35);
}
html[data-theme='light'] .arch-diagram {
  box-shadow: 0 4px 18px rgba(0, 0, 0, 0.07);
}
.arch-diagram svg {
  display: block;
  /* 放得下时居中；放不下（min-width 生效）时 margin auto 归零 ⇒ 左对齐、两端可滚 */
  margin: 0 auto;
  max-width: 100%;
  height: auto;
  min-width: 720px;
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
    margin: -8px 0 0;
    font-size: 12.5px;
    text-align: center;
    color: var(--vp-c-text-mute);
  }
}
.arch-diagram .band {
  fill-opacity: 0.055;
  stroke-opacity: 0.6;
  stroke-width: 1.4;
}
html[data-theme='dark'] .arch-diagram .band { fill: #ffffff; }
html[data-theme='light'] .arch-diagram .band { fill: #000000; }
.arch-diagram .band-title {
  font-size: 13px;
  font-weight: 700;
  letter-spacing: 0.5px;
}
.arch-diagram .node rect {
  fill-opacity: 0.1;
  stroke-width: 1.4;
  stroke-opacity: 0.85;
}
.arch-diagram .node-accent rect {
  fill-opacity: 0.17;
  stroke-width: 2;
}
.arch-diagram .node-text {
  fill: var(--ad-text);
  font-size: 14px;
  font-weight: 600;
}
.arch-diagram .node-sub {
  fill: var(--ad-text2);
  font-size: 11.5px;
}
.arch-diagram .node.small .node-text {
  font-size: 13px;
}
.arch-diagram .edge path {
  fill: none;
  stroke: var(--ad-line);
  stroke-width: 1.6;
  marker-end: url(#ad-arrow);
  opacity: 0.9;
}
</style>
