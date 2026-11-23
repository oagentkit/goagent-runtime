#!/usr/bin/env node
// site/scripts/gen_reference_pages.mjs —— 站点事实生成器（机器事实 / 门禁体系 / 坑速查）
//
// 为什么要有这个生成器（与 kernel 站 site/gen.mjs、矩阵站 scripts/gen_module_pages.py 同一口径）：
// 站点上的每个数字（包数、行数、依赖版本、CI 作业清单）都来自仓库，手抄一遍就等于多一份
// 会静默失真的真相。本仓已经有实物：
//   - README 的「包」表只列了 9 个包，磁盘上其实有 10 个（containerbackend 没进表）；
//   - 站点 packages/README.md 开头写「八个包」，与 README 的九个、磁盘的十个是第三套口径。
// 生成页把这件事变成**机器可见的事实**（对账表），而不是靠人记得更新。
//
// 失败形态是**变红**：任何一处解析不到都以退出码 1 失败，不静默填默认值。
// 坑速查数据文件里的 kernel 地板版本与 go.mod 对不上 ⇒ 同样判红。
//
// 用法：
//   node site/scripts/gen_reference_pages.mjs          # 生成
//   node site/scripts/gen_reference_pages.mjs --check  # 只校验是否已是最新（CI 用）

import { existsSync, mkdirSync, readFileSync, readdirSync, statSync, writeFileSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'

const HERE = dirname(fileURLToPath(import.meta.url))
const SITE = join(HERE, '..')
const REPO = join(SITE, '..')
const OUT = join(SITE, 'src', 'reference')
const ADR_OUT = join(SITE, 'src', 'adr')
const PUBLIC = join(SITE, 'src', '.vuepress', 'public')
const MANIFEST = join(SITE, 'src', '.vuepress', 'adr-manifest.json')
const DATA = join(SITE, 'data', 'pitfalls.json')
const KERNEL_MOD = 'github.com/yi-nology/goagent-kernel'

const read = (p) => readFileSync(p, 'utf8')
const readRepo = (p) => read(join(REPO, p))

// 自测模式下 fail 抛错而不是退出——否则自测没法断言「解析失效必须判红」。
let selftestMode = false
function fail(msg) {
  if (selftestMode) throw new Error('SELFTEST_FAIL: ' + msg)
  console.error('gen_reference_pages.mjs: ' + msg)
  process.exit(1)
}

const bt = String.fromCharCode(96) // markdown code span 用的反引号
const code = (s) => bt + s + bt
// markdown 表格单元格转义：竖线会截断表格
const cell = (s) => String(s).replace(/\|/g, '\\|')
// 尖括号转义（code span 外）：VuePress 把 markdown 编译成 Vue 模板，
// 裸 <x> 会被当自定义元素解析，「Element is missing end tag」直接炸构建。
const escAngles = (s) =>
  String(s)
    .split(bt)
    .map((seg, i) => (i % 2 ? seg : seg.replace(/</g, '&lt;').replace(/>/g, '&gt;')))
    .join(bt)

// ── go.mod ────────────────────────────────────────────────────────────────
function loadGoMod(src) {
  const module = (src.match(/^module\s+(\S+)/m) || [])[1]
  if (!module) fail('go.mod 解析不到 module 指令')
  const go = (src.match(/^go\s+(\S+)/m) || [])[1]
  if (!go) fail('go.mod 解析不到 go 指令')
  const requires = []
  let inBlock = false
  for (const raw of src.split('\n')) {
    const line = raw.trim()
    if (!line) continue
    if (/^require\s*\($/.test(line)) {
      inBlock = true
      continue
    }
    if (inBlock && line === ')') {
      inBlock = false
      continue
    }
    let spec = null
    if (inBlock) spec = line
    else if (/^require\s+\S/.test(line)) spec = line.replace(/^require\s+/, '')
    else continue
    const parts = spec.split(/\s+/)
    if (parts.length < 2 || !parts[1].startsWith('v')) continue
    requires.push({ module: parts[0], version: parts[1], indirect: spec.includes('// indirect') })
  }
  if (!requires.length) fail('go.mod 解析出 0 个依赖——解析失效')
  return { module, go, requires }
}

// ── 磁盘上的包（机器事实的底座）────────────────────────────────────────
function isStdlib(p) {
  return !p.split('/')[0].includes('.')
}

function moduleOfImport(p, goMod) {
  if (p === goMod.module || p.startsWith(goMod.module + '/')) return null
  let best = null
  for (const r of goMod.requires) {
    if (p === r.module || p.startsWith(r.module + '/')) {
      if (!best || r.module.length > best.module.length) best = r
    }
  }
  if (best) return best.module
  if (isStdlib(p)) return null
  return p // 未登记在 go.mod 的外部依赖：如实显示，不藏
}

function collectImports(files) {
  const set = new Set()
  for (const f of files) {
    const src = read(f)
    for (const m of src.matchAll(/import\s*\(([\s\S]*?)\)/g)) {
      for (const line of m[1].split('\n')) {
        const im = line.match(/^\s*(?:[\w.]+\s+)?"([^"]+)"/)
        if (im) set.add(im[1])
      }
    }
    for (const m of src.matchAll(/^import\s+(?:[\w.]+\s+)?"([^"]+)"/gm)) set.add(m[1])
  }
  return set
}

function packageDoc(files) {
  for (const f of files) {
    const m = read(f).match(/^\/\/ Package\s+\S+\s+(.*)$/m)
    if (m) return m[1].trim()
  }
  return ''
}

function listDiskPackages(goMod) {
  const out = []
  for (const name of readdirSync(REPO).sort()) {
    if (name.startsWith('.') || name.startsWith('_')) continue
    if (name === 'site' || name === 'examples') continue
    const dir = join(REPO, name)
    if (!statSync(dir).isDirectory()) continue
    const all = readdirSync(dir).filter((f) => f.endsWith('.go'))
    if (!all.length) continue
    const prod = all.filter((f) => !f.endsWith('_test.go'))
    const files = prod.map((f) => join(dir, f))
    let lines = 0
    for (const f of files) {
      for (const line of read(f).split('\n')) if (line.trim()) lines++
    }
    const deps = new Set()
    for (const p of collectImports(files)) {
      const m = moduleOfImport(p, goMod)
      if (m) deps.add(m)
    }
    out.push({
      name,
      prodFiles: prod.length,
      prodLines: lines,
      deps: [...deps].sort(),
      doc: packageDoc(files),
    })
  }
  if (!out.length) fail('磁盘上解析出 0 个包——枚举失效')
  return out
}

// ── README 的「包」表 ────────────────────────────────────────────────────
function loadReadmePackages(src) {
  const sec = src.split('\n## 包\n')[1]?.split('\n## ')[0]
  if (!sec) fail('README 找不到「## 包」段——结构变了？')
  const rows = []
  for (const line of sec.split('\n')) {
    if (!line.trim().startsWith('|')) continue
    const cells = line
      .trim()
      .replace(/^\|/, '')
      .replace(/\|$/, '')
      .split('|')
      .map((c) => c.trim())
    if (cells.length < 2) continue
    // 一行可能写多个包（复合行）：逐个反引号名取出
    const names = [...cells[0].matchAll(/`([^`]+)`/g)].map((m) => m[1])
    if (!names.length) continue
    for (const name of names) rows.push({ name, desc: cells[1] })
  }
  if (!rows.length) fail('README 的「包」表解析出 0 行——解析失效')
  return rows
}

// ── Makefile 目标 ───────────────────────────────────────────────────────
function loadMakeTargets(src) {
  const out = []
  let cur = null
  for (const raw of src.split('\n')) {
    if (!raw.trim() || raw.trim().startsWith('#')) continue
    if (!/^\s/.test(raw)) {
      const m = /^([A-Za-z0-9_.-]+):(.*)$/.exec(raw)
      if (m && !m[1].startsWith('.')) {
        cur = { name: m[1], deps: m[2].trim(), cmds: [] }
        out.push(cur)
        continue
      }
      cur = null
      continue
    }
    if (cur && /^\t/.test(raw)) cur.cmds.push(raw.replace(/^\t+/, '').trim())
  }
  if (!out.length) fail('Makefile 解析出 0 个目标——解析失效')
  return out
}

// ── CI 作业 ─────────────────────────────────────────────────────────────
function loadCiJobs(src) {
  const lines = src.split('\n')
  const jobsIdx = lines.findIndex((l) => /^jobs:\s*$/.test(l))
  if (jobsIdx < 0) fail('ci.yml 找不到 jobs 段——结构变了？')
  const jobs = []
  let cur = null
  let inSteps = false
  let inServices = false
  let pending = null
  let envKeys = null

  const flush = () => {
    if (!pending) return
    pending.step.run = pending.lines.join('\n')
    pending = null
  }

  for (let i = jobsIdx + 1; i < lines.length; i++) {
    const raw = lines[i]
    if (!raw.trim() || raw.trim().startsWith('#')) continue
    const indent = raw.length - raw.trimStart().length
    const t = raw.trim()
    // run: | 的块内容：缩进比步骤更深 ⇒ 归上一个 run
    if (pending && indent > 6) {
      pending.lines.push(t)
      continue
    }
    flush()
    if (indent === 2 && /^[\w-]+:\s*$/.test(t)) {
      cur = { id: t.slice(0, -1), name: '', runsOn: '', timeout: '', services: [], steps: [] }
      jobs.push(cur)
      inSteps = false
      inServices = false
      continue
    }
    if (!cur) continue
    if (indent === 4) {
      inSteps = false
      inServices = false
      envKeys = null
      const nm = /^name:\s*(.*)$/.exec(t)
      const ro = /^runs-on:\s*(.*)$/.exec(t)
      const tm = /^timeout-minutes:\s*(.*)$/.exec(t)
      if (nm) cur.name = nm[1].trim()
      else if (ro) cur.runsOn = ro[1].trim()
      else if (tm) cur.timeout = tm[1].trim()
      else if (/^services:\s*$/.test(t)) inServices = true
      else if (/^steps:\s*$/.test(t)) inSteps = true
      continue
    }
    if (inServices && indent === 6) {
      cur.services.push({ name: t.slice(0, -1), image: '' })
      continue
    }
    if (inServices && indent === 8) {
      const im = /^image:\s*(.*)$/.exec(t)
      if (im && cur.services.length) cur.services[cur.services.length - 1].image = im[1].trim()
      continue
    }
    if (inSteps && indent === 6 && t.startsWith('- ')) {
      const body = t.slice(2).trim()
      const step = { label: '', run: '', env: [] }
      cur.steps.push(step)
      envKeys = null
      const lm = /^(name|uses):\s*(.*)$/.exec(body)
      const rm = /^run:\s*(.*)$/.exec(body)
      if (lm) step.label = lm[2].trim()
      else if (rm) {
        if (rm[1].trim() === '|' || rm[1].trim() === '|-') pending = { step, lines: [] }
        else step.run = rm[1].trim()
      }
      continue
    }
    if (inSteps && indent === 8) {
      const step = cur.steps[cur.steps.length - 1]
      const nm = /^name:\s*(.*)$/.exec(t)
      const rm = /^run:\s*(.*)$/.exec(t)
      const em = /^env:\s*$/.exec(t)
      if (nm) step.label = nm[1].trim()
      else if (rm) {
        if (rm[1].trim() === '|' || rm[1].trim() === '|-') pending = { step, lines: [] }
        else step.run = rm[1].trim()
      } else if (em) envKeys = step.env
      else if (envKeys) envKeys.push(t)
      continue
    }
    if (inSteps && indent === 10 && envKeys) {
      envKeys.push(t)
      continue
    }
  }
  flush()

  // 交叉校验：解析出的作业数 / 步骤数必须与文件里的字面量一致。
  // 不一致 = 有内容没被解析到（缩进变了 / key 改名）⇒ 当场红，
  // 而不是生成一份少几条作业的清单（少一条的清单读起来完全正常）。
  //
  // 关键：校验器与解析器**故意不同源**。
  // 校验器数「块头」（后面跟着更深缩进的键），键名不限字符集；
  // 解析器只认 GitHub Actions 的合法字符集（[\w-]+）且写死缩进 2。
  // 两者若用同一个条件，就等于自己判自己——漂移时一起变绿，
  // 而「判据变绿」正是本仓反复说的那类最贵的失效。
  const tail = lines.slice(jobsIdx + 1)
  const blockHeaders = []
  for (let i = 0; i < tail.length; i++) {
    const m = /^(\s+)([^:\s][^:]*):\s*$/.exec(tail[i])
    if (!m) continue
    const ind = m[1].length
    if (ind < 2) continue
    const next = tail.slice(i + 1).find((x) => x.trim() !== '')
    if (!next) continue
    if (next.length - next.trimStart().length > ind) blockHeaders.push(ind)
  }
  if (!blockHeaders.length) fail('ci.yml 的 jobs 段解析不出块头——结构变了？')
  const firstLevel = Math.min.apply(null, blockHeaders)
  const rawJobs = blockHeaders.filter((ind) => ind === firstLevel).length
  const rawSteps = tail.filter((l) => /^ {6}- /.test(l)).length
  const parsedSteps = jobs.reduce((n, j) => n + j.steps.length, 0)
  if (rawJobs !== jobs.length) {
    fail('ci.yml 作业数对不上：块头 ' + rawJobs + ' 个，解析出 ' + jobs.length + ' 个（缩进或 key 变了？）')
  }
  if (rawSteps !== parsedSteps) {
    fail('ci.yml 步骤数对不上：字面量 ' + rawSteps + ' 个，解析出 ' + parsedSteps + ' 个（缩进或 key 变了？）')
  }
  if (!jobs.length) fail('ci.yml 解析出 0 个作业——解析失效')
  return jobs
}

// ── 页：机器事实 ────────────────────────────────────────────────────────
function renderPackages(goMod, disk, readmePkgs) {
  const readmeMap = new Map(readmePkgs.map((p) => [p.name, p.desc]))
  const rows = disk.map((p) => ({
    ...p,
    desc: readmeMap.get(p.name) || p.doc || '（仓内无职责描述）',
    from: readmeMap.has(p.name) ? 'README 包表' : '包文档注释',
  }))
  const onlyDisk = disk.map((p) => p.name).filter((n) => !readmeMap.has(n))
  const onlyReadme = readmePkgs.map((p) => p.name).filter((n) => !disk.some((d) => d.name === n))
  const direct = goMod.requires.filter((r) => !r.indirect)
  const indirect = goMod.requires.filter((r) => r.indirect)
  const pureGoSqlite = goMod.requires.some((r) => r.module === 'modernc.org/sqlite')
  const cgoSqlite = goMod.requires.some((r) => r.module === 'github.com/mattn/go-sqlite3')

  const L = [
    '---',
    'title: 机器事实（包 × 依赖 × 能力）',
    '---',
    '',
    '# 机器事实（包 × 依赖 × 能力）',
    '',
    '::: warning 本页由 ' + code('site/scripts/gen_reference_pages.mjs') + ' 生成，勿手改',
    '包清单、生产行数与仓外依赖取自**磁盘上的仓**（包目录 / import / go.mod）；',
    '职责优先取自 README「包」表，缺项回落到包文档注释。',
    '每次 ' + code('npm run build') + ' 自动重新生成（CI 用 ' + code('--check') + ' 判同步）。',
    '要改事实请改源头文件——**手抄一遍就等于多一份会静默失真的真相**。',
    ':::',
    '',
    '## 包清单（' + rows.length + ' 个包）',
    '',
    '依赖列只列仓外 import；「无」表示只用标准库与本仓包。',
    '生产行数 = 非测试 .go 文件的非空行数（含注释）。',
    code('examples/') + ' 下是可执行示例，不计入包清单。',
    '',
    '| 包 | 生产文件 | 生产行数 | 仓外依赖 | 职责 | 职责来源 |',
    '|---|---|---|---|---|---|',
    ...rows.map(
      (p) =>
        '| ' +
        code(p.name) +
        ' | ' +
        p.prodFiles +
        ' | ' +
        p.prodLines +
        ' | ' +
        cell(p.deps.length ? p.deps.map(code).join('、') : '无') +
        ' | ' +
        cell(escAngles(p.desc)) +
        ' | ' +
        p.from +
        ' |'
    ),
    '',
    '## README 包表 ⇄ 磁盘目录（对账）',
    '',
    '两处清单若对不上，**站点上就会出现两套互相矛盾的包清单**——这正是本页存在的理由。',
    '',
    '| 包 | 在 README「包」表 | 在磁盘 |',
    '|---|---|---|',
    ...rows.map(
      (p) =>
        '| ' + code(p.name) + ' | ' + (readmeMap.has(p.name) ? '✓' : '✗') + ' | ✓ |'
    ),
    ...onlyReadme.map((n) => '| ' + code(n) + ' | ✓ | ✗ |'),
    '',
    onlyDisk.length || onlyReadme.length
      ? '**当前有 ' +
        (onlyDisk.length + onlyReadme.length) +
        ' 处对不上**：' +
        (onlyDisk.length ? '磁盘有而 README 表没有 → ' + onlyDisk.map(code).join('、') + '；' : '') +
        (onlyReadme.length ? 'README 表有而磁盘没有 → ' + onlyReadme.map(code).join('、') + '；' : '') +
        '改完源头文件重跑生成器，本段会自动收口。'
      : '两处清单**完全一致**（' + rows.length + '/' + rows.length + '）。',
    '',
    '## 依赖（go.mod）',
    '',
    '直接依赖 ' + direct.length + ' 个，间接依赖 ' + indirect.length + ' 个。',
    '',
    '| 模块 | 版本 | 直接 / 间接 |',
    '|---|---|---|',
    ...goMod.requires.map(
      (r) => '| ' + code(r.module) + ' | ' + code(r.version) + ' | ' + (r.indirect ? '间接' : '直接') + ' |'
    ),
    '',
    '## 底座',
    '',
    '| 指标 | 值 |',
    '|---|---|',
    '| 模块路径 | ' + code(goMod.module) + ' |',
    '| Go 指令 | ' + code(goMod.go) + ' |',
    '| SQLite 驱动 | ' +
      (pureGoSqlite
        ? code('modernc.org/sqlite') + '（纯 Go）⇒ ' + code('CGO_ENABLED=0') + ' 可编'
        : cgoSqlite
          ? code('github.com/mattn/go-sqlite3') + '（需 CGO）'
          : '未登记驱动') +
      ' |',
    '| 判据归属 | 状态机 / 幂等闸 / 预算 / 恢复 / 抢占接管全部来自 ' + code('goagent-kernel') + '，本仓只做接线 |',
    '',
  ]
  return L.join('\n')
}

// ── 页：门禁体系 ────────────────────────────────────────────────────────
function renderGates(goMod, jobs, targets) {
  const L = [
    '---',
    'title: 门禁体系',
    '---',
    '',
    '# 门禁体系：判据在 CI 里，不在自觉',
    '',
    '::: warning 本页由 ' + code('site/scripts/gen_reference_pages.mjs') + ' 生成，勿手改',
    '作业清单与命令取自 ' + code('.github/workflows/ci.yml') + '，本地目标取自 ' + code('Makefile') + '；',
    '每次 ' + code('npm run build') + ' 自动重新生成。',
    ':::',
    '',
    '## CI 作业（' + jobs.length + ' 个）',
    '',
    '| 作业 | 名称 | 判据 | 外部服务 |',
    '|---|---|---|---|',
    ...jobs.map((j) => {
      const items = j.steps.map((s) => {
        const raw = s.label || s.run || '（无标签）'
        const label = raw.startsWith('actions/') ? '准备 ' + raw : raw
        const body = s.run ? ' → ' + code(s.run.replace(/\n/g, ' ; ')) : ''
        const env = s.env.length ? '（env: ' + code(s.env.map((e) => e.split(':')[0].trim()).join(', ')) + '）' : ''
        return escAngles(label) + body + env
      })
      const svc = j.services.length
        ? j.services.map((s) => s.name + (s.image ? '（' + s.image + '）' : '')).join('、')
        : '无'
      return (
        '| ' + code(j.id) + ' | ' + cell(escAngles(j.name || '（无）')) + ' | ' + items.join('<br>') + ' | ' + svc + ' |'
      )
    }),
    '',
    '> 判据的**本体**不在本仓：存储语义跑 ' + code('goagent-kernel') + ' 的共享契约套件（' +
      code('sidetest') + '），抢占 / 租约 / 接管代次跑 ' + code('taskboard') +
      ' 契约，幂等闸跑 ' + code('sideeffect') + '。本仓只做接线，不另写第二套判据。',
    '',
    '## 本地等价命令（Makefile）',
    '',
    '| 目标 | 依赖 | 命令 |',
    '|---|---|---|',
    ...targets.map(
      (t) =>
        '| ' +
        code(t.name) +
        ' | ' +
        (t.deps ? cell(code(t.deps)) : '—') +
        ' | ' +
        (t.cmds.length ? t.cmds.map((c) => code(c)).join('<br>') : '（组合目标）') +
        ' |'
    ),
    '',
  ]
  return L.join('\n')
}

// ── 页：坑速查 ──────────────────────────────────────────────────────────
function renderPitfalls(data, goMod) {
  const kernel = goMod.requires.find((r) => r.module === KERNEL_MOD)
  if (!kernel) fail('go.mod 里找不到 ' + KERNEL_MOD + ' 依赖——地板版本无从对账')
  if (data.kernelFloor !== kernel.version) {
    fail(
      '坑速查数据文件的 kernelFloor（' +
        data.kernelFloor +
        '）与 go.mod 的实际版本（' +
        kernel.version +
        '）对不上——改数据文件，或改 go.mod 后重新生成'
    )
  }
  if (!Array.isArray(data.sections) || !data.sections.length) fail('坑速查数据文件没有 sections')
  for (const sec of data.sections) {
    if (!sec.heading || !Array.isArray(sec.rows) || !sec.rows.length) fail('坑速查数据文件的 section 结构不完整')
    for (const r of sec.rows) {
      if (!r.symptom || !r.cause || !r.fix) fail('坑速查数据文件有一行缺字段（症状 / 真因 / 处置）')
    }
  }

  const L = [
    '---',
    'title: 坑速查',
    '---',
    '',
    '# 坑速查：症状 → 真因 → 处置',
    '',
    '::: warning 本页由 ' + code('site/scripts/gen_reference_pages.mjs') + ' 从 ' + code('site/data/pitfalls.json') + ' 生成，勿手改',
    '坑表的唯一真相是 ' + code('site/data/pitfalls.json') + '；数据文件里的 kernel 地板版本（' +
      code(data.kernelFloor) +
      '）与 go.mod 对账，对不上即判红。',
    ':::',
    '',
    '本仓踩过的坑与它的前提都在这儿。接完线拿它自查一遍。',
    '',
  ]
  for (const sec of data.sections) {
    L.push('## ' + sec.heading, '')
    L.push('| 症状 | 真因 | 处置 |', '|---|---|---|')
    for (const r of sec.rows) {
      L.push('| ' + cell(escAngles(r.symptom)) + ' | ' + cell(escAngles(r.cause)) + ' | ' + cell(escAngles(r.fix)) + ' |')
    }
    L.push('')
  }
  return L.join('\n')
}

// ── ADR（决策记录）──────────────────────────────────────────────────────
//
// ADR 是**手写的仓内真相源**（docs/adr/），站点只做投影——
// 与 kernel 站 site/gen.mjs 同一口径：仓内是真相，站点不手抄第二份。
function parseAdr(raw, name) {
  const id = (raw.match(/^# ADR-(\d{4})/) || [])[1]
  if (!id) fail(name + '：文件头不含 ADR-NNNN 编号')
  const h1 = raw.split('\n').find((l) => l.startsWith('# ADR-'))
  if (!h1) fail(name + '：找不到一级标题（# ADR-NNNN · 标题）')
  const title = h1.replace(/^#\s*ADR-\d{4}\s*·\s*/, '').trim()
  const sum = (raw.match(/^> \*\*一句话结论\*\*：(.+)$/m) || [])[1]
  if (!sum) fail(name + '：找不到「一句话结论」——索引与站点摘要都要用它')
  const st = (raw.match(/^\| \*\*状态\*\* \| (.+) \|$/m) || [])[1]
  if (!st) fail(name + '：找不到「状态」行')
  return { id: 'ADR-' + id, title, summary: sum.trim(), status: st.trim(), file: name, raw }
}

function loadAdrs() {
  const dir = join(REPO, 'docs', 'adr')
  let names
  try {
    names = readdirSync(dir).filter((n) => /^ADR-\d{4}-.*\.md$/.test(n)).sort()
  } catch (e) {
    fail('读不到 docs/adr/ 目录：' + e.message)
  }
  if (!names.length) fail('docs/adr/ 下没有 ADR-*.md——ADR 目录被清空了？')
  return names.map((name) => parseAdr(read(join(dir, name)), name))
}

// ── 版本记录（从 CHANGELOG.md 投影）────────────────────────────────────
function loadReleases(src) {
  const out = []
  let cur = null
  for (const line of src.split('\n')) {
    const h = /^## \[(v[\d.]+)\] — (.+)$/.exec(line)
    if (h) {
      cur = { version: h[1], date: h[2].trim(), subject: '' }
      out.push(cur)
      continue
    }
    if (cur && !cur.subject) {
      // 家族生成器有两种渲染：conventional commit 走「### 标题」小标题，
      // 其余走「- 条目」列表——两种都要认，否则主题会空着（读起来像漏了内容）。
      const t = /^### (.+)$/.exec(line)
      const b = /^-\s+(.+)$/.exec(line)
      if (t) cur.subject = t[1].trim()
      else if (b) cur.subject = b[1].trim()
    }
  }
  if (!out.length) fail('CHANGELOG.md 解析出 0 个版本段——段头格式变了？')
  return out
}
// ── 页：决策记录（ADR）─────────────────────────────────────────────────
const GH_REPO = 'https://github.com/yi-nology/goagent-runtime'

function renderAdrIndex(adrs) {
  const L = [
    '---',
    'title: 决策记录（ADR）',
    '---',
    '',
    '# 决策记录（ADR）',
    '',
    '::: warning 本页由 ' + code('site/scripts/gen_reference_pages.mjs') + ' 从仓内 ' + code('docs/adr/') + ' 生成，勿手改',
    '决策记录是**设计推导与取舍**的真相源；README 只留结论，推导一律在仓内 ADR。',
    ':::',
    '',
    '| 编号 | 决策 | 一句话结论 | 状态 |',
    '|---|---|---|---|',
    ...adrs.map(
      (a) =>
        '| [' + a.id + '](/goagent-runtime/adr/' + a.file.replace(/\.md$/, '.html') + ') | ' +
        cell(a.title) + ' | ' + cell(escAngles(a.summary)) + ' | ' + cell(a.status) + ' |'
    ),
    '',
    '> 仓内索引：' + GH_REPO + '/blob/main/docs/adr/README.md（新增 ADR 的流程写在那里）。',
    ''
  ]
  return L.join('\n')
}

function renderAdrPage(a) {
  const body = a.raw.split('\n').slice(1).join('\n')
  return [
    '---',
    'title: ' + a.id + ' · ' + a.title,
    'description: ' + a.summary,
    '---',
    '',
    escAngles(body),
    ''
  ].join('\n')
}

// ── 页：版本记录 ───────────────────────────────────────────────────────
function renderReleases(releases) {
  const L = [
    '---',
    'title: 版本记录',
    '---',
    '',
    '# 版本记录',
    '',
    '::: warning 本页由 ' + code('site/scripts/gen_reference_pages.mjs') + ' 从仓内 ' + code('CHANGELOG.md') + ' 投影生成，勿手改',
    '版本 / 日期 / 主题投影自仓内 ' + code('CHANGELOG.md') + '（该文件由家族工具 ' + code('matrix-tools/scripts/gen_changelogs.py') + ' 从 git 历史生成）；',
    '逐版完整变更看仓内 ' + code('CHANGELOG.md') + '。',
    ':::',
    '',
    '| 版本 | 日期 | 主题 |',
    '|---|---|---|',
    ...releases.map(
      (r) =>
        '| ' + code(r.version) + ' | ' + cell(r.date) + ' | ' + cell(escAngles(r.subject || '（见 CHANGELOG）')) + ' |'
    ),
    '',
    '> 完整变更：仓内 [CHANGELOG.md](' + GH_REPO + '/blob/main/CHANGELOG.md)。',
    ''
  ]
  return L.join('\n')
}
// ── AI 直读入口：llms.txt（同样生成，数字不手抄）───────────────────────
// 与 kernel / 矩阵站的 llms.txt 同一形态：给 AI 代理的紧凑事实页。
function renderLlms(goMod, disk, readmePkgs, releases, adrs) {
  const site = 'https://yi-nology.github.io/goagent-runtime'
  const gh = GH_REPO
  const readmeMap = new Map(readmePkgs.map((p) => [p.name, p.desc]))
  const short = (s) => {
    const cut = String(s || '').split(/[；。]/)[0]
    return (cut.length > 60 ? cut.slice(0, 60) + '…' : cut).trim()
  }
  const kernel = goMod.requires.find((r) => r.module === KERNEL_MOD)
  const prodFiles = disk.reduce((n, p) => n + p.prodFiles, 0)
  const prodLines = disk.reduce((n, p) => n + p.prodLines, 0)
  const direct = goMod.requires.filter((r) => !r.indirect).length
  const indirect = goMod.requires.filter((r) => r.indirect).length
  const L = [
    '# goagent-runtime',
    '',
    '> goagent-kernel 的**运行时装配层**（Go library）：把 kernel 的确定性机制',
    '> （状态 / 幂等 / 预算 / 恢复）装配成「一个普通 Go 团队不读几十个包也能用」的运行时。',
    '> **判据不另写一套**——状态机 / 幂等闸 / 回合守卫 / 恢复 / 抢占接管全部来自 goagent-kernel，',
    '> 本仓只做接线。',
    '> 文档站: ' + site + '  |  GitHub: ' + gh,
    '',
    '> 本文件由 ' + code('site/scripts/gen_reference_pages.mjs') + ' 生成，数字取自 README / go.mod /',
    '> Makefile / ci.yml / CHANGELOG.md / docs/adr，随构建刷新。',
    '',
    '## 判定口诀',
    '',
    '「这个判断，kernel 是不是已经判过了？」是 → 用 kernel 的判据，本仓只接线。',
    '',
    '## 四个必须先知道的约束',
    '',
    '1. **判据归 kernel，本仓只接线**：状态机 / 幂等闸 / 预算 / 恢复 / 抢占接管代次全在 kernel；新存储实现必须跑 kernel 的 sidetest 共享契约套件。',
    '2. **模型接缝只有 ' + code('chat.Model') + ' 一个窄接口**：供应商中立，本仓不 import 任何具体 LLM SDK；适配在宿主侧。',
    '3. **恢复出来的历史要么完整要么没有这一段**：半截历史 fail-closed 报错不猜；结果未知显式上抛 ' + code('res.Uncertain') + '，禁止盲目重试。',
    '4. **cluster 的「接管不重复施加副作用」有前提**：靠宿主声明业务幂等键 ' + code('pipeline.Tool.IdempotencyKey') + '（如订单号）；未声明时跨副本这一步依赖模型给出稳定 tool_call_id。',
    '',
    '## AI 直读入口（同族）',
    '- goagent-kernel（编排内核）: https://yi-nology.github.io/goagent-kernel/llms.txt',
    '- oekit（基础库矩阵）: https://oekit.github.io/llms.txt',
    '- oagentkit（Agent/LLM 编排工具链矩阵）: https://oagentkit.github.io/llms.txt',
    '',
    '## 文档（人读）',
    '- 五分钟上手: ' + site + '/guide/quickstart.html',
    '- 架构总览: ' + site + '/guide/architecture.html',
    '- 核心概念: ' + site + '/guide/concepts.html',
    '- 包导览: ' + site + '/packages/',
    '- 模板选型（embedded / standalone / cluster）: ' + site + '/templates/',
    '- 崩溃恢复语义: ' + site + '/advanced/recovery.html',
    '- 坑速查（症状→真因→处置）: ' + site + '/reference/pitfalls.html',
    '- 机器事实（包 × 依赖 × 能力）: ' + site + '/reference/packages.html',
    '- 门禁体系: ' + site + '/reference/gates.html',
    '- 决策记录（ADR，设计推导与取舍）: ' + site + '/reference/adr.md',
    '- 版本记录: ' + site + '/reference/releases.html',
    '- 完整仓内事实: ' + gh + '#readme',
    '',
    '## 事实快照',
    '- 模块: ' + code(goMod.module) + ' · Go ' + code(goMod.go) + ' · MIT',
    '- 能力包 ' + disk.length + ' 个（生产代码 ' + prodLines + ' 行 / ' + prodFiles + ' 个文件）',
    '- kernel 地板版本: ' + code(kernel ? kernel.version : '未登记') + '；判据全部来自 kernel 契约面',
    '- 第一方依赖 ' + direct + ' 个（直接）/ ' + indirect + ' 个（间接）',
    '- 模板: ' + code('NewEmbedded') + '（内存）/ ' + code('NewStandalone') + '（SQLite 单文件）/ ' + code('cluster') + '（PG + worker 池，多副本）',
    '- 已发布版本 ' + releases.length + ' 个，最新 ' + code(releases[0].version) + '；决策记录 ' + adrs.length + ' 篇',
    '',
    '## 包一览（' + disk.length + '）',
    ...disk.map((p) => '- ' + code(p.name) + ' — ' + short(readmeMap.get(p.name) || p.doc)),
    ''
  ]
  return L.join('\n')
}
// ── 自测：解析判据的反向验证 ────────────────────────────────────────────
//
// 为什么要有这个：生成器产出的每个数字都进到站点的「机器事实」页，而
// **解析失效的失败形态是静默的**——某条内容没被解析到时，页面只是少一行，
// 读起来完全正常，没有任何东西会红。这正是本仓反复说的那类病：判据变绿。
//
// 做法：直接调用**本文件里真正在跑的解析函数**（不复制实现——副本与实现一漂移，
// 用例就退化成「看着在、其实没判」），喂构造输入，断言「该红的时候必须红」。
function selftestParse() {
  selftestMode = true
  const results = []
  const ok = (name, cond) => results.push({ name, ok: !!cond })
  const mustFail = (name, fn) => {
    try {
      fn()
      ok(name, false)
    } catch (err) {
      ok(name, String(err && err.message).indexOf('SELFTEST_FAIL') >= 0)
    }
  }

  // ① ci.yml：jobs 段整体改缩进 ⇒ 解析器认不出作业、校验器数得出 ⇒ 必须红
  // （这条是「校验器与解析器不同源」的证据：两者同源时，漂移会一起变绿）
  const realCi = readRepo('.github/workflows/ci.yml')
  mustFail('ci.yml jobs 段改缩进必须判红（少一条作业的清单读起来完全正常）', () =>
    loadCiJobs(realCi.replace(/^ {2}([\w-]+):/gm, '    $1:'))
  )
  mustFail('ci.yml 缺 jobs 段必须判红', () => loadCiJobs('name: ci\n'))

  // ② go.mod：解析不到 module 指令 ⇒ 必须红
  mustFail('go.mod 缺 module 指令必须判红', () => loadGoMod('go 1.27.4\n'))

  // ③ README：包表段改名 ⇒ 必须红（解析失效会静默生成空表）
  mustFail('README 缺「## 包」段必须判红', () => loadReadmePackages('# 没有包表\n'))

  // ④ Makefile：解析不出目标 ⇒ 必须红
  mustFail('Makefile 无目标必须判红', () => loadMakeTargets('# 只有注释\n'))

  // ⑤ ADR：缺「一句话结论」/「状态」⇒ 必须红
  mustFail('ADR 缺「一句话结论」必须判红', () =>
    parseAdr('# ADR-0009 · 样例\n\n| 项 | 内容 |\n|---|---|\n| **状态** | 已采纳 |\n', 'x.md')
  )
  mustFail('ADR 缺「状态」行必须判红', () =>
    parseAdr('# ADR-0009 · 样例\n\n> **一句话结论**：结论\n', 'x.md')
  )

  // ⑥ CHANGELOG：段头格式变了 ⇒ 必须红
  mustFail('CHANGELOG 解析不出版本段必须判红', () => loadReleases('# 没有版本段\n'))

  // ⑦ 坑表：kernel 地板版本与 go.mod 对不上 ⇒ 必须红
  const goMod = loadGoMod(readRepo('go.mod'))
  mustFail('坑表 kernel 地板漂移必须判红', () =>
    renderPitfalls(
      { kernelFloor: 'v0.99.0', sections: [{ heading: 'x', rows: [{ symptom: 'a', cause: 'b', fix: 'c' }] }] },
      goMod
    )
  )

  // ⑧ 正向：真实仓内文件必须解析得出非空结果（防止「全都判红」也算通过）
  const adrs = loadAdrs()
  const releases = loadReleases(readRepo('CHANGELOG.md'))
  const jobs = loadCiJobs(realCi)
  ok('真实 ADR 至少一篇', adrs.length >= 1)
  ok('真实 CHANGELOG 至少一个版本段', releases.length >= 1)
  ok('真实 ci.yml 至少一个作业', jobs.length >= 1)
  ok('真实 ADR 都带一句话结论与状态', adrs.every((a) => a.summary && a.status))

  selftestMode = false
  const bad = results.filter((r) => !r.ok)
  for (const r of results) console.log((r.ok ? '  ✅ ' : '  ❌ ') + r.name)
  if (bad.length) {
    console.error('gen_reference_pages.mjs: 自测有 ' + bad.length + ' 项未通过')
    process.exit(1)
  }
  console.log('自测通过：' + results.length + ' 项（含「该红的时候必须红」的负向用例）')
}

// ── 主流程 ──────────────────────────────────────────────────────────────
function main() {
  const check = process.argv.includes('--check')
  if (process.argv.includes('--selftest')) return selftestParse()

  const goMod = loadGoMod(readRepo('go.mod'))
  const disk = listDiskPackages(goMod)
  const readmePkgs = loadReadmePackages(readRepo('README.md'))
  const targets = loadMakeTargets(readRepo('Makefile'))
  const jobs = loadCiJobs(readRepo('.github/workflows/ci.yml'))
  const data = JSON.parse(read(DATA))
  const adrs = loadAdrs()
  const releases = loadReleases(readRepo('CHANGELOG.md'))

  const expected = new Map()
  expected.set(join(OUT, 'packages.md'), renderPackages(goMod, disk, readmePkgs))
  expected.set(join(OUT, 'gates.md'), renderGates(goMod, jobs, targets))
  expected.set(join(OUT, 'pitfalls.md'), renderPitfalls(data, goMod))
  expected.set(join(OUT, 'adr.md'), renderAdrIndex(adrs))
  expected.set(join(OUT, 'releases.md'), renderReleases(releases))
  // 侧栏清单交给配置去读：页数会变，清单必须由生成物给出，
  // 否则每加一篇 ADR 都要手改一次配置（而手改的那次没人会记得）。
  expected.set(
    MANIFEST,
    JSON.stringify(adrs.map((a) => ({ file: a.file, id: a.id, title: a.title })), null, 2) + '\n'
  )
  for (const a of adrs) expected.set(join(ADR_OUT, a.file), renderAdrPage(a))
  expected.set(join(PUBLIC, 'llms.txt'), renderLlms(goMod, disk, readmePkgs, releases, adrs))

  if (check) {
    const stale = []
    for (const [p, content] of expected) {
      if (!existsSync(p) || read(p) !== content) stale.push(p)
    }
    if (stale.length) {
      console.error('以下产物与仓库事实不同步，请跑 npm run gen:reference：')
      for (const p of stale) console.error('  - ' + p)
      process.exit(1)
    }
    console.log('OK：' + expected.size + ' 个生成产物与仓库事实同步')
    return
  }

  for (const [p, content] of expected) {
    mkdirSync(dirname(p), { recursive: true })
    writeFileSync(p, content)
    console.log('wrote ' + p)
  }
}

main()
