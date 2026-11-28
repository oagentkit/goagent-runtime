import { viteBundler } from '@vuepress/bundler-vite'
import { defineUserConfig } from 'vuepress'
import { defaultTheme } from '@vuepress/theme-default'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { readFileSync } from 'node:fs'

const here = path.dirname(fileURLToPath(import.meta.url))

// ADR 侧栏清单由生成器产出（site/scripts/gen_reference_pages.mjs）。
//
// 为什么不写死在本文件里：每加一篇 ADR 就要手改一次侧栏配置，
// 而那一次手改总会有人忘——侧栏与实际页数就此漂移，且没有任何东西会红。
// 故清单从生成物读：页数变了，配置自动跟着变。
//
// 清单文件不存在时直接报错而不是静默用空数组：那是构建顺序错了
// （npm run build 会先跑生成器），不是「本仓没有 ADR」。
let adrManifest: { file: string; id: string; title: string }[] = []
try {
  adrManifest = JSON.parse(readFileSync(new URL('./adr-manifest.json', import.meta.url), 'utf8'))
} catch (err) {
  console.error('config.ts: 读不到 adr-manifest.json —— 先跑 npm run gen:reference（npm run build 会自动先跑）')
  throw err
}

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
    {
      // 锚点落位偏移 ↔ 框架「当前章节」判定对齐（详见 client.ts 顶部注释）。
      //
      // 主题的 active-header-links 插件用 __AHL_OFFSET__（默认 5）判定「当前所在
      // 章节」：章节起点 = 该标题的 offsetTop。而我们的落位把标题放在**导航栏
      // 下方**（scroll-margin-top = 导航栏高 58 + 余量 12 = 70px）⇒ 落位点落在
      // **上一节**里 ⇒ 插件立刻把地址栏的 hash 改写成上一节 ⇒ 深链的锚点被抹掉。
      //
      // 这里把该常量调到 ≥ 落位偏移（取 120，留余量），插件的章节边界就与落位
      // 口径一致 ⇒ 地址栏的 hash 停在读者要的锚点上（侧栏高亮也跟着对）。
      //
      // 为什么是覆盖常量而不是改插件选项：主题只把该插件的**开关**暴露成
      // themePlugins（选项写死在主题里，改不了 offset）⇒ 只能覆盖它编译期读的
      // 常量（define 由 bundler 替换）。
      name: 'runtime-anchor-offset',
      define: { __AHL_OFFSET__: 120 },
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
      // 本仓的 llms.txt 由 site/scripts/gen_reference_pages.mjs 从仓内事实生成
      // （不手搓一份会漂移的生成物），所以「本站」这一项现在确实存在。
      {
        text: 'AI 直读',
        children: [
          { text: '本站 llms.txt', link: 'https://yi-nology.github.io/goagent-runtime/llms.txt' },
          { text: 'kernel llms.txt', link: 'https://yi-nology.github.io/goagent-kernel/llms.txt' },
          { text: 'oekit llms.txt', link: 'https://oekit.github.io/llms.txt' },
          { text: 'oagentkit llms.txt', link: 'https://oagentkit.github.io/llms.txt' },
        ],
      },
      // 参考区：与 kernel 站 / 矩阵站同一个入口、同一个标题。
      // 「版本记录」投影自仓内 CHANGELOG.md，「决策记录」投影自仓内 docs/adr/
      // ——两者都有事实源，不是凑出来的空条目。
      {
        text: '参考',
        children: [
          { text: '机器事实（包 × 依赖 × 能力）', link: '/reference/packages.html' },
          { text: '门禁体系', link: '/reference/gates.html' },
          { text: '决策记录（ADR）', link: '/reference/adr.html' },
          { text: '版本记录', link: '/reference/releases.html' },
        ],
      },
    ],
    footer: 'MIT License · github.com/yi-nology/goagent-runtime · <a href="https://yi-nology.github.io/goagent-runtime/llms.txt">AI 直读 llms.txt</a>',
    sidebar: {
      '/guide/': [
        { text: '入门', children: ['/guide/', '/guide/quickstart.html', '/guide/entries.html', '/guide/architecture.html', '/guide/concepts.html'] },
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
        {
          text: '参考',
          children: [
            '/reference/packages.html',
            '/reference/gates.html',
            '/reference/adr.html',
            '/reference/releases.html',
            '/reference/pitfalls.html',
          ],
        },
      ],
      '/adr/': [
        { text: '决策记录', children: adrManifest.map((a) => '/adr/' + a.file.replace(/\.md$/, '.html')) },
      ],
    },
  }),
})
