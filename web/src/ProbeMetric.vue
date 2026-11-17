<script setup lang="ts">
import {computed} from 'vue'
import type {ProbeHistoryPoint} from './api'
import ProbeTrend from './ProbeTrend.vue'
const props=defineProps<{points:ProbeHistoryPoint[];kind:'cpu'|'memory'|'disk';label:string;value:number;ready:boolean;detail?:string;error?:string}>()
const accent=computed(()=>({cpu:'var(--el-color-primary)',memory:'var(--el-color-primary)',disk:'var(--el-color-primary)'})[props.kind])
const series=computed(()=>[{key:props.kind,label:props.label,color:accent.value}])
</script>
<template>
 <div class="probe-meter">
  <div class="probe-meter-heading"><span>{{label}}</span><strong :style="{color:accent}">{{ready?value.toFixed(2)+'%':'—'}}</strong></div>
  <div v-if="ready" class="probe-meter-track" role="progressbar" :aria-label="`${label}使用率`" aria-valuemin="0" aria-valuemax="100" :aria-valuenow="Math.max(0,Math.min(100,value))"><span class="probe-meter-fill" :style="{width:Math.max(0,Math.min(100,value))+'%',background:accent}"></span></div>
  <div v-else class="probe-meter-track" aria-hidden="true"></div>
  <ProbeTrend :points="points" :series="series" :label="kind" percentage :error="error"/>
  <small v-if="detail">{{detail}}</small>
 </div>
</template>
