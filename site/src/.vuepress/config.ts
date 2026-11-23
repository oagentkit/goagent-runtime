import { viteBundler } from '@vuepress/bundler-vite'
import { defineUserConfig } from 'vuepress'
import { defaultTheme } from '@vuepress/theme-default'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const here = path.dirname(fileURLToPath(import.meta.url))

export default defineUserConfig({
  bundler: viteBundler(),
  base: '/goagent-runtime/',
  lang: 'zh-CN',
  title: 'goagent-runtime',
  description: 'goagent-kernel 的运行时装配层：Loop / Pipeline / 存储 / 事件的接线与默认口径',
  head: [
    // 主题色跟随系统的明暗偏好：固定一个值总有一半模式对不上
    // （浏览器地址栏是深色而页面是亮色，反之亦然）。
    ['meta', { name: 'theme-color', content: '#ffffff', media: '(prefers-color-scheme: light)' }],
    ['meta', { name: 'theme-color', content: '#0d1117', media: '(prefers-color-scheme: dark)' }],
  ],
  extendsMarkdown: (md) => {
    const fence = md.renderer.rules.fence
    md.renderer.rules.fence = (tokens, idx, options, env, self) => {
      const token = tokens[idx]
      if (token.info.trim() === 'mermaid') {
        return '<Mermaid code="' + encodeURIComponent(token.content) + '" />\n'
      }
      return fence!(tokens, idx, options, env, self)
    }
  },
  // 用别名覆盖把「白 / 暗 / 自动」三态开关换进顶栏。
  //
  // 为什么必须覆盖主题自带的那个按钮：VuePress 2 的原生开关只有两态
  // （亮 ⇄ 暗），「自动」只是**首次访问**时的取值来源（prefers-color-scheme），
  // 用户手动切过之后就再也回不到「跟随系统」。而主题内部的存储模型本来
  // 就是三态（localStorage['vuepress-color-scheme'] ∈ auto|light|dark），
  // 缺的只是一个能显式选中「自动」的界面入口 ⇒ 这里补的是**入口**。
  //
  // 为什么用别名覆盖而不是改主题源码：别名是 VuePress 的公开机制
  // （app.pluginApi.hooks.alias）。用户插件在主题插件**之后**注册
  // ⇒ 同名键后者覆盖前者；vite 侧按键长降序匹配 ⇒ 具体的
  // '@theme/VPToggleColorModeButton.vue' 一定先于 '@theme' 前缀命中。
  plugins: [
    {
      name: 'runtime-theme-mode-switch',
      alias: {
        '@theme/VPToggleColorModeButton.vue': path.resolve(here, './components/ThemeModeSwitch.vue'),
      },
    },
  ],
  theme: defaultTheme({
    logo: null,
    repo: 'yi-nology/goagent-runtime',
    docsDir: 'site/src',
    // 白 / 暗 / 自动三态（2026-11-23 改，与 kernel 站 / 矩阵站同一套）。
    // colorMode: 'auto' ⇒ 首次访问按 prefers-color-scheme 定；用户手动切过后
    // 走 localStorage。colorModeSwitch: true ⇒ 顶栏出切换按钮——原生那个
    // 只有两态，三态入口由上面 plugins 的别名覆盖提供。
    colorMode: 'auto',
    colorModeSwitch: true,
    editLinkText: '在 GitHub 上编辑此页',
    lastUpdatedText: '最后更新',
    contributorsText: '贡献者',
    tip: '提示',
    warning: '注意',
    danger: '警告',
    notFound: ['页面不存在'],
    backToHome: '返回首页',
    navbar: [
      { text: '指南', link: '/guide/' },
      { text: '包', link: '/packages/' },
      { text: '模板', link: '/templates/' },
      { text: '进阶', link: '/advanced/recovery.html' },
      { text: '参与贡献', link: '/dev/' },
      // 坑速查提到顶栏一级：三站主导航同层（与 kernel 站 / 矩阵站同一形状）。
      { text: '坑速查', link: '/reference/pitfalls.html' },
      // 站群配套链接：与 kernel 站 / 矩阵站同一个入口、同一个标题。
      // 本仓**没有**自己的 llms.txt（不手搓一份会漂移的生成物），
      // 所以这里只列族内确实存在的三个。
      {
        text: 'AI 直读',
        children: [
          { text: 'kernel llms.txt', link: 'https://yi-nology.github.io/goagent-kernel/llms.txt' },
          { text: 'oekit llms.txt', link: 'https://oekit.github.io/llms.txt' },
          { text: 'oagentkit llms.txt', link: 'https://oagentkit.github.io/llms.txt' },
        ],
      },
      // 参考区：与 kernel 站 / 矩阵站同一个入口、同一个标题。
      // 本仓没有 CHANGELOG 与 ADR ⇒ 不硬凑「版本记录 / 决策记录」两项
      // （那两项没有事实源，凑出来就是第二份会漂移的真相）。
      {
        text: '参考',
        children: [
          { text: '机器事实（包 × 依赖 × 能力）', link: '/reference/packages.html' },
          { text: '门禁体系', link: '/reference/gates.html' },
        ],
      },
    ],
    footer: 'MIT License · github.com/yi-nology/goagent-runtime · <a href="https://yi-nology.github.io/goagent-kernel/llms.txt">AI 直读 llms.txt</a>',
    sidebar: {
      '/guide/': [
        { text: '入门', children: ['/guide/', '/guide/quickstart.html', '/guide/architecture.html', '/guide/concepts.html'] },
      ],
      '/packages/': [
        { text: '包导览', children: ['/packages/', '/packages/chat.html', '/packages/pipeline.html', '/packages/agentloop.html', '/packages/storage.html', '/packages/sqlite-pg.html', '/packages/runtime.html', '/packages/observe.html'] },
      ],
      '/templates/': [
        { text: '模板', children: ['/templates/', '/templates/embedded.html', '/templates/standalone.html'] },
      ],
      '/advanced/': [
        { text: '进阶', children: ['/advanced/recovery.html'] },
      ],
      '/dev/': [
        { text: '参与贡献', children: ['/dev/'] },
      ],
      '/reference/': [
        { text: '参考', children: ['/reference/packages.html', '/reference/gates.html', '/reference/pitfalls.html'] },
      ],
    },
  }),
})
