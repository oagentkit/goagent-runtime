import { viteBundler } from '@vuepress/bundler-vite'
import { defineUserConfig } from 'vuepress'
import { defaultTheme } from '@vuepress/theme-default'

export default defineUserConfig({
  bundler: viteBundler(),
  base: '/goagent-runtime/',
  lang: 'zh-CN',
  title: 'goagent-runtime',
  description: 'goagent-kernel 的运行时装配层：Loop / Pipeline / 存储 / 事件的接线与默认口径',
  head: [
    ['meta', { name: 'theme-color', content: '#3eaf7c' }],
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
  theme: defaultTheme({
    logo: null,
    repo: 'yi-nology/goagent-runtime',
    docsDir: 'site/src',
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
    ],
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
    },
  }),
})
