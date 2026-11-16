<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import Icon from './Icon.vue'
import type { Rule } from './api'

type DiagnosisNode = { id: string; kind: 'ingress' | 'egress' | 'target'; name: string; address: string }
type DiagnosisLink = { source: string; target: string; status: 'reachable' | 'unreachable' | 'unknown' | 'not_checked'; latency_ms?: number; reason?: string }
export type Diagnosis = { rule_id: number; protocol: string; nodes: DiagnosisNode[]; links: DiagnosisLink[] }
const props = defineProps<{ rule: Rule; load: () => Promise<Diagnosis> }>()
const emit = defineEmits<{ close: [] }>()
const result = ref<Diagnosis | null>(null), loading = ref(false), error = ref('')
const nodeNames: Record<DiagnosisNode['kind'], string> = { ingress: '入口', egress: '出口', target: '目标' }
const states: Record<DiagnosisLink['status'], string> = { reachable: '可达', unreachable: '不可达', unknown: '无法确认', not_checked: '未检测' }
function linkText(link: DiagnosisLink) {
  return link.status === 'reachable' && link.latency_ms != null ? `${Number(link.latency_ms.toFixed(1))} ms` : states[link.status] || '未检测'
}
async function run() {
  if (loading.value) return
  loading.value = true
  error.value = ''
  try { result.value = await props.load() } catch (e) { result.value = null; error.value = (e as Error).message || '诊断失败，请重试。' } finally { loading.value = false }
}
onMounted(run)

const graph = computed(() => {
  const nodes = result.value?.nodes || [], links = result.value?.links || []
  const middle = !!(props.rule.egress_node_id || props.rule.egress_group_id)
  const kinds: DiagnosisNode['kind'][] = middle ? ['ingress', 'egress', 'target'] : ['ingress', 'target']
  const columns = kinds.map(kind => nodes.filter(n => n.kind === kind))
  const columnLinks = kinds.slice(0, -1).map((kind, i) => links.filter(link => nodes.some(n => n.id === link.source && n.kind === kind) && nodes.some(n => n.id === link.target && n.kind === kinds[i + 1])))
  // Give every link a separate label lane, even when multiple links cross.
  const rows = Math.max(3, ...columns.map(c => c.length * 2), ...columnLinks.map(c => c.length))
  const height = Math.max(240, 120 + rows * 33), width = middle ? 1040 : 780, cardWidth = 174
  const x = middle ? [24, 433, 842] : [24, 582]
  const yFor = (index: number, total: number) => total === 1 ? height / 2 + 13 : 80 + index * (height - 142) / (total - 1)
  const placed = columns.flatMap((column, c) => column.map((node, i) => ({ ...node, x: x[c], y: yFor(i, column.length), column: c })))
  const positions = new Map(placed.map(node => [node.id, node]))
  const edges = columnLinks.flatMap((column, c) => column.flatMap((link, i) => {
    const from = positions.get(link.source), to = positions.get(link.target)
    if (!from || !to) return []
    const start = from.x + cardWidth, end = to.x, mid = (start + end) / 2, labelY = yFor(i, column.length)
    return [{ ...link, key: `${c}:${i}:${link.source}:${link.target}`, from, to, mid, labelY, d: `M ${start} ${from.y} C ${start + 40} ${from.y} ${mid - 50} ${labelY} ${mid} ${labelY} C ${mid + 50} ${labelY} ${end - 40} ${to.y} ${end} ${to.y}` }]
  }))
  return { width, height, placed, edges, x, kinds, cardWidth }
})
const names = computed(() => new Map((result.value?.nodes || []).map(node => [node.id, node])))
function shortText(value: string, maxUnits: number) {
  // CJK glyphs are roughly twice as wide as Latin letters at the same font size.
  const chars = Array.from(value), units = (char: string) => /^[\x00-\x7f]$/.test(char) ? .55 : 1
  if (chars.reduce((sum, char) => sum + units(char), 0) <= maxUnits) return value
  let text = '', used = 0
  for (const char of chars) { const size = units(char); if (used + size > maxUnits - 1) break; text += char; used += size }
  return `${text}…`
}
</script>

<template>
  <el-dialog :model-value="true" title="规则诊断" class="diagnosis-dialog" @close="emit('close')">
    <div class="diagnosis-heading"><div><strong>{{ rule.name }}</strong><span class="diagnosis-number">#{{ rule.id }} · {{ (rule.protocol || 'tcp').toUpperCase() }}</span></div><el-button :loading="loading" @click="run"><Icon name="refresh" />重新诊断</el-button></div>
    <p class="diagnosis-subtitle">{{ rule.egress_node_id || rule.egress_group_id ? '入口 → 出口 → 目标' : '入口 → 目标' }} · 按链路分别检测</p>
    <el-alert v-if="error" :title="error" type="error" :closable="false" show-icon />
    <div v-if="loading && !result" class="diagnosis-loading" role="status">正在诊断链路，请稍候…</div>
    <template v-if="result">
      <div class="diagnosis-scroll" :aria-busy="loading">
        <svg class="diagnosis-graph" :width="graph.width" :height="graph.height" :viewBox="`0 0 ${graph.width} ${graph.height}`" role="img" aria-label="规则诊断拓扑图，详细结果见下方链路结果">
          <g v-for="(kind, i) in graph.kinds" :key="kind"><text :x="graph.x[i] + graph.cardWidth / 2" y="32" text-anchor="middle" class="diagnosis-column">{{ nodeNames[kind] }}</text></g>
          <g v-for="edge in graph.edges" :key="edge.key" :class="`diagnosis-edge ${edge.status}`"><path :d="edge.d" /><circle :cx="edge.from.x + graph.cardWidth" :cy="edge.from.y" r="3" /><circle :cx="edge.to.x" :cy="edge.to.y" r="3" /><title>{{ edge.from.name }} → {{ edge.to.name }}：{{ linkText(edge) }}{{ edge.reason ? ' · ' + edge.reason : '' }}</title></g>
          <g v-for="edge in graph.edges" :key="edge.key + '-label'" :class="`diagnosis-label ${edge.status}`"><rect :x="edge.mid - 40" :y="edge.labelY - 12" width="80" height="24" rx="12" /><text :x="edge.mid" :y="edge.labelY + 4" text-anchor="middle">{{ linkText(edge) }}</text></g>
          <g v-for="node in graph.placed" :key="node.id" class="diagnosis-node"><rect :x="node.x" :y="node.y - 24" :width="graph.cardWidth" height="48" rx="12" /><circle :cx="node.x + 18" :cy="node.y" r="5" /><text :x="node.x + 32" :y="node.y - (node.address ? 3 : -4)" class="diagnosis-node-name">{{ shortText(node.name, 9) }}</text><text v-if="node.address" :x="node.x + 32" :y="node.y + 14" class="diagnosis-node-address">{{ shortText(node.address, 11) }}</text><title>{{ node.name }}{{ node.address ? ' · ' + node.address : '' }}</title></g>
        </svg>
      </div>
      <div class="diagnosis-legend"><span><i class="reachable" />可达</span><span><i class="unreachable" />不可达</span><span><i class="unknown" />无法确认 / 未检测</span><span v-if="loading" role="status">正在重新诊断…</span></div>
      <details class="diagnosis-details"><summary>查看链路结果（{{ result.links.length }}）</summary><ul><li v-for="(link, index) in result.links" :key="index"><span>{{ names.get(link.source)?.name || link.source }} → {{ names.get(link.target)?.name || link.target }}</span><strong :class="link.status">{{ linkText(link) }}</strong><span v-if="link.reason" class="diagnosis-reason">{{ link.reason }}</span></li></ul></details>
      <p class="diagnosis-note">{{ result.protocol === 'udp' ? 'UDP 无响应时无法确认可达性；诊断结果不代表实际业务连通性。' : '时间为各链路检测耗时，不代表用户端的实际访问延迟。' }}</p>
    </template>
    <template #footer><el-button @click="emit('close')">关闭</el-button></template>
  </el-dialog>
</template>

<style>
.diagnosis-dialog { width: min(1120px, calc(100vw - 32px)); }
.diagnosis-dialog .el-dialog__body { padding-top: 8px; }
.diagnosis-heading { display: flex; align-items: center; justify-content: space-between; gap: 12px; }
.diagnosis-heading strong { font-size: 16px; }
.diagnosis-number, .diagnosis-subtitle, .diagnosis-note { color: var(--muted); font-size: 13px; }
.diagnosis-number { margin-left: 10px; }
.diagnosis-subtitle { margin: 8px 0 18px; }
.diagnosis-loading { min-height: 190px; display: grid; place-items: center; color: var(--muted); }
.diagnosis-scroll { max-height: min(54vh, 560px); overflow: auto; border: 1px solid var(--border); border-radius: 14px; background: var(--el-fill-color-lighter); }
.diagnosis-graph { display: block; min-width: 0; }
.diagnosis-column { fill: var(--muted); font-size: 13px; font-weight: 600; }
.diagnosis-node rect { fill: var(--surface); stroke: var(--border); stroke-width: 1.5; }
.diagnosis-node circle { fill: var(--el-color-primary); }
.diagnosis-node-name { fill: var(--el-text-color-primary); font-size: 14px; font-weight: 600; }
.diagnosis-node-address { fill: var(--muted); font-size: 11px; }
.diagnosis-edge { color: #94a3b8; }
.diagnosis-edge path { fill: none; stroke: currentColor; stroke-width: 2; stroke-dasharray: 3 5; }
.diagnosis-edge circle { fill: currentColor; }
.diagnosis-edge.reachable, .diagnosis-label.reachable { color: #13855b; }
.diagnosis-edge.unreachable, .diagnosis-label.unreachable { color: #d63d4e; }
.diagnosis-edge.unknown, .diagnosis-edge.not_checked, .diagnosis-label.unknown, .diagnosis-label.not_checked { color: #77889e; }
.diagnosis-label rect { fill: var(--surface); stroke: currentColor; stroke-opacity: .25; }
.diagnosis-label text { fill: currentColor; font-size: 12px; font-weight: 600; }
.diagnosis-legend { display: flex; flex-wrap: wrap; gap: 14px; margin-top: 12px; color: var(--muted); font-size: 12px; }
.diagnosis-legend span { display: inline-flex; align-items: center; gap: 6px; }
.diagnosis-legend i { width: 8px; height: 8px; border-radius: 50%; background: #77889e; }
.diagnosis-legend i.reachable { background: #13855b; }.diagnosis-legend i.unreachable { background: #d63d4e; }
.diagnosis-details { margin-top: 18px; font-size: 13px; }
.diagnosis-details summary { cursor: pointer; color: var(--el-text-color-primary); font-weight: 600; }
.diagnosis-details ul { list-style: none; margin: 10px 0 0; padding: 0; }
.diagnosis-details li { display: flex; flex-wrap: wrap; align-items: baseline; gap: 5px 12px; padding: 8px 0; border-bottom: 1px solid var(--border); }
.diagnosis-details li > span:first-child { min-width: min(260px, 100%); }
.diagnosis-details strong.reachable { color: #13855b; }.diagnosis-details strong.unreachable { color: #d63d4e; }
.diagnosis-reason { color: var(--muted); overflow-wrap: anywhere; }
.diagnosis-note { line-height: 1.6; margin: 16px 0 0; }
.dark .diagnosis-edge.reachable, .dark .diagnosis-label.reachable, .dark .diagnosis-details strong.reachable { color: #64d8aa; }
.dark .diagnosis-edge.unreachable, .dark .diagnosis-label.unreachable, .dark .diagnosis-details strong.unreachable { color: #ff8a99; }
.dark .diagnosis-edge.unknown, .dark .diagnosis-edge.not_checked, .dark .diagnosis-label.unknown, .dark .diagnosis-label.not_checked { color: #a8b7cb; }
.dark .diagnosis-legend i.reachable { background: #64d8aa; }.dark .diagnosis-legend i.unreachable { background: #ff8a99; }
@media (max-width: 600px) { .diagnosis-dialog { margin-top: 6vh; }.diagnosis-heading { align-items: flex-start; }.diagnosis-number { display: block; margin: 4px 0 0; } }
</style>
