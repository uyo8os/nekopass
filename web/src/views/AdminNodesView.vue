<script setup lang="ts">
import {ref} from 'vue'
import PageShell from '../PageShell.vue'
import NodesPage from '../NodesPage.vue'
import {usePage} from '../page'
import {api,type Node,type NodeGroup} from '../api'
withDefaults(defineProps<{view?:'nodes'|'groups'}>(),{view:'nodes'})
const nodes=ref<Node[]>([]),groups=ref<NodeGroup[]>([])
let sortGeneration=0
const {me,loading,error,refresh}=usePage(async()=>{const generation=sortGeneration;const[n,g]=await Promise.all([api<Node[]>('admin/nodes'),api<NodeGroup[]>('admin/node-groups')]);if(generation===sortGeneration){nodes.value=n;groups.value=g}},{admin:true,interval:5000})
</script>
<template><PageShell :me="me" :loading="loading" :error="error" active="nodes" admin><NodesPage :nodes="nodes" :groups="groups" :view="view" @invalidate-refresh="sortGeneration++" @refresh="refresh" /></PageShell></template>
