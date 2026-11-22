#!/usr/bin/env node
// site/scripts/check-links.mjs —— 站点内部链接门禁（防「子路径部署 ⇒ 根相对链接 404」复发）
//
// 为什么要有这条门禁：站点部署在**子路径**（base = '/goagent-runtime/'），而 markdown 里的
// 链接会被 markdown-it 输出成**原生 <a href>** —— 框架不加工它 ⇒ 写 '/guide/x.html' 时
// 浏览器解析成 https://yi-nology.github.io/guide/x.html（少一层前缀）⇒ **404**。
// 实录 2026-11-23：站点首页「接下来」三条链接全部 404，导航/hero 按钮却正常
// （导航与 frontmatter actions 走框架的路由链接，运行时会被加上 base）。
//
// 两条判据（两类链接的语义不同，不能同判）：
//   A. 正文链接（markdown ](...) 与 HTML href="..."）：
//      内部链接必须**自带 base 前缀**，且目标页面必须存在。
//   B. 路由链接（frontmatter 的 link: 与 config.ts 的 nav/sidebar link:）：
//      必须是**路由路径**（以 '/' 开头且**不含** base）——框架运行时会给它加 base；
//      写成带 base 的会被加两次（/goagent-runtime/goagent-runtime/...）。
//      目标页面同样必须存在。
//
// 失败形态是**变红**：解析不到 base、找不到页面、链接形态不对，一律非零退出。

import { readFileSync, readdirSync, existsSync, statSync } from 'node:fs'
import { join, dirname, resolve, relative } from 'node:path'
import { fileURLToPath } from 'node:url'

const HERE = dirname(fileURLToPath(import.meta.url))
const SITE = resolve(HERE, '..')
const SRC = join(SITE, 'src')
const CONFIG = join(SRC, '.vuepress', 'config.ts')

function fail(msg) {
  console.error('❌ ' + msg)
  process.exit(1)
}

function readBase() {
  let txt
  try {
    txt = readFileSync(CONFIG, 'utf8')
  } catch (e) {
    fail('读不到 ' + CONFIG + '：' + e.message)
  }
  const m = txt.match(/^\s*base:\s*['"`]([^'"`]+)['"`]/m)
  if (!m) fail('config.ts 里解析不到 base —— 门禁判据跑不起来，按红处理（不猜）')
  const base = m[1]
  if (!base.startsWith('/') || !base.endsWith('/')) fail('base 必须以 / 开头并以 / 结尾：' + base)
  return base
}

const base = readBase()

function walk(dir, out = []) {
  for (const name of readdirSync(dir)) {
    if (name === '.vuepress' || name === 'node_modules' || name === 'dist') continue
    const p = join(dir, name)
    if (statSync(p).isDirectory()) walk(p, out)
    else if (name.endsWith('.md')) out.push(p)
  }
  return out
}

function isExternal(u) {
  return /^(https?:|mailto:|tel:|data:|javascript:|\/\/|#)/i.test(u)
}

// base 之后的路径 → 源文件是否存在
function pageExists(relPath) {
  const rel = relPath.replace(/^\/+/, '')
  const cands = []
  if (rel === '' || rel.endsWith('/')) cands.push(rel + 'README.md')
  else if (rel.endsWith('.html')) cands.push(rel.replace(/\.html$/, '.md'))
  else cands.push(rel + '.md', rel + '/README.md')
  return cands.some((c) => existsSync(join(SRC, c)))
}

const violations = []
function report(file, line, url, why) {
  violations.push({ file: relative(SITE, file), line, url, why })
}

const mdFiles = walk(SRC)

for (const file of mdFiles) {
  const raw = readFileSync(file, 'utf8')
  const lines = raw.split('\n')
  const text = raw

  // --- frontmatter 的路由链接（判据 B）---
  const fm = text.match(/^---\r?\n([\s\S]*?)\r?\n---/)
  if (fm) {
    fm[1].split('\n').forEach((l, i) => {
      const m = l.match(/^\s*link:\s*['"`]([^'"`]+)['"`]/)
      if (!m) return
      const url = m[1]
      if (isExternal(url)) return
      if (url.startsWith(base)) {
        report(file, i + 2, url, '路由链接写成了带 base 的形态——框架运行时还会再加一次 base')
      } else if (!url.startsWith('/')) {
        report(file, i + 2, url, '路由链接必须以 / 开头（相对路径在路由里不可解析）')
      } else if (!pageExists(url)) {
        report(file, i + 2, url, '路由链接指向的页面不存在')
      }
    })
  }

  // --- 正文链接（判据 A）---
  const patterns = [
    /\]\(([^)\s]+)(?:\s+["'][^"']*["'])?\)/g, // markdown ](...)
    /href="([^"]+)"/g, // HTML href="..."
    /href='([^']+)'/g,
  ]
  for (const re of patterns) {
    let m
    while ((m = re.exec(text)) !== null) {
      const url = m[1]
      if (isExternal(url)) continue
      const line = text.slice(0, m.index).split('\n').length
      if (url.startsWith(base)) {
        if (!pageExists(url.slice(base.length - 1))) {
          report(file, line, url, '带 base 的正文链接指向的页面不存在')
        }
      } else if (url.startsWith('/')) {
        report(file, line, url, '正文链接是原生 <a>，框架不加工 base；子路径部署下必须自带 "' + base + '" 前缀')
      } else {
        report(file, line, url, '正文内部链接必须用带 base 的绝对路径（相对路径在无尾斜杠的 URL 下会跳错目录）')
      }
    }
  }
}

// --- config.ts 的 nav/sidebar 路由链接（判据 B）---
{
  const raw = readFileSync(CONFIG, 'utf8')
  raw.split('\n').forEach((l, i) => {
    const m = l.match(/link:\s*['"`]([^'"`]+)['"`]/)
    if (!m) return
    const url = m[1]
    if (isExternal(url)) return
    if (url.startsWith(base)) {
      report(CONFIG, i + 1, url, '路由链接写成了带 base 的形态——框架运行时还会再加一次 base')
    } else if (!url.startsWith('/')) {
      report(CONFIG, i + 1, url, '路由链接必须以 / 开头')
    } else if (!pageExists(url)) {
      report(CONFIG, i + 1, url, '路由链接指向的页面不存在')
    }
  })
}

if (violations.length) {
  console.error('站点内部链接门禁：红 ' + violations.length + ' 处')
  for (const v of violations) {
    console.error('  ' + v.file + ':' + v.line + '  ' + v.url + '\n      ↳ ' + v.why)
  }
  process.exit(1)
}

console.log('✅ 站点内部链接门禁通过：' + mdFiles.length + ' 个页面，正文链接均自带 base 前缀，路由链接均为纯路由路径，目标页面全部存在')
