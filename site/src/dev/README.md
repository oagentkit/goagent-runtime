# 参与贡献

## 本地构建文档站

本站由 VuePress 2（2.0.0-rc.32）生成，源码在仓库 site/ 目录：

```bash
cd site
npm install          # 首次
npm run dev          # 本地预览（http://localhost:8080/goagent-runtime/）
npm run build        # 产出到 site/src/.vuepress/dist
```

## 文档写作约定

- **图表用 mermaid**：在 markdown 里写 mermaid 围栏代码块（三个反引号 + mermaid）即可，站点内置渲染组件；
- **小白优先**：每页开头一句「一句话职责」，概念页先讲「解决什么事故」再讲定义；
- **依赖与调用关系必须配图**：包页至少一张依赖图 + 一张时序/流程图；
- **如实声明边界与前提**：能力写清它解决什么、依赖什么前提（如 cluster 接管后不重复施加副作用靠业务幂等键）、证据写在哪；不吹，也不留没兑现的自白。

## 部署

GitHub Actions 在 main 分支 site/ 变更时自动构建并发布到 GitHub Pages（工作流见 .github/workflows/pages.yml）。手动验证产物：npm run build 后看 site/src/.vuepress/dist。
