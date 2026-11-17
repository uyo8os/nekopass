<script setup lang="ts">
import {computed} from 'vue'
import type {ProbeHistoryPoint} from './api'
import {probePath,probeScale,type ProbeSeries} from './probe-chart'
const props=withDefaults(defineProps<{points:ProbeHistoryPoint[];series:ProbeSeries[];label:string;percentage?:boolean;error?:string}>(),{percentage:false,error:''})
const scale=computed(()=>probeScale(props.points,props.series.map(s=>s.key),props.percentage))
const curves=computed(()=>props.series.map(series=>({...series,...probePath(props.points,series.key,scale.value,0)})))
const anyData=computed(()=>curves.value.some(c=>c.line))
const span=computed(()=>curves.value[0]?.span||60)
const chartLabel=computed(()=>({cpu:'CPU',memory:'内存',disk:'硬盘',io:'主磁盘 I/O'} as Record<string,string>)[props.label]||props.label)
</script>
<template>
 <div class="probe-trend" :data-testid="`probe-chart-${label}`">
  <svg viewBox="0 0 310 86" preserveAspectRatio="none" role="img" :aria-label="`${chartLabel}趋势图，最近 ${Math.round(span)} 秒${anyData?'':'，等待采样'}`">
   <line x1="0" x2="310" y1="78" y2="78" class="probe-chart-grid"/>
   <g v-for="curve in curves" :key="curve.key">
    <path v-if="percentage&&curve.area" :d="curve.area" :fill="curve.color" fill-opacity=".12"/>
    <path v-if="curve.line" :data-series="curve.key" :d="curve.line" fill="none" :stroke="curve.color" stroke-width="2" stroke-linejoin="round" vector-effect="non-scaling-stroke"/>
    <circle v-if="curve.last" :cx="curve.last[0]" :cy="curve.last[1]" r="1.8" :fill="curve.color"/>
   </g>
  </svg>
  <span v-if="!anyData" class="probe-trend-empty">等待采样</span>
  <span v-if="error" class="probe-trend-error" :title="error">历史暂不可用，显示当前采样</span>
 </div>
</template>
