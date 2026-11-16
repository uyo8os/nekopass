import assert from 'node:assert/strict'
import { test } from 'node:test'
import { sharedEgressChoices, sharedPortRange } from '../web/src/egress-options.ts'

const node = (id, extra = {}) => ({ id, name: `node${id}`, ingress_enabled: true, allow_direct: true, tunnel_exit_enabled: false, allowed_ingress_ids: [], tunnel_protocol: 'plain_tcp', port_min: 80, port_max: 65535, ...extra })
const entries = [node(1,{port_min:80,port_max:9000}),node(2,{port_min:443,port_max:10000})]
const exits = [node(3,{tunnel_exit_enabled:true,allowed_ingress_ids:[1,2]}),node(4,{tunnel_exit_enabled:true,allowed_ingress_ids:[1]})]

test('group exit choices must be available to every ingress', () => {
 const all = [...entries,...exits]
 const groups = [{id:7,name:'common',strategy:'random',node_ids:[3]},{id:8,name:'one-sided',strategy:'random',node_ids:[4]}]
 assert.deepEqual(sharedEgressChoices(all,groups,[1,2],'tcp').map(x=>x.value),['node:0','node:3','group:7'])
 assert.deepEqual(sharedEgressChoices(all,groups,[],'tcp'),[])
 assert.deepEqual(sharedEgressChoices([entries[0],{...entries[1],allow_direct:false},...exits],groups,[1,2],'tcp').map(x=>x.value),['node:3','group:7'])
})

test('port range is the intersection of all ingress ranges', () => {
 assert.deepEqual(sharedPortRange(entries),{min:443,max:9000})
 assert.equal(sharedPortRange([]),null)
 assert.deepEqual(sharedPortRange([entries[0],node(5,{port_min:10000})]),{min:10000,max:9000})
})
