import { defineClientConfig } from 'vuepress/client'
import Mermaid from './components/Mermaid.vue'
import ArchDiagram from './components/ArchDiagram.vue'

export default defineClientConfig({
  enhance({ app }) {
    app.component('Mermaid', Mermaid)
    app.component('ArchDiagram', ArchDiagram)
  },
})
