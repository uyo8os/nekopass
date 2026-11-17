// Build web first. Requires Playwright on NODE_PATH and optionally NEKOPASS_BROWSER.
// Runs against local fixture APIs; no live panel or accounts are accessed.
const assert=require('node:assert/strict');
const http=require('node:http');
const fs=require('node:fs');
const path=require('node:path');
const {chromium}=require('playwright');
const root=path.resolve(__dirname,'../web/dist');
const server=http.createServer((req,res)=>{
 let name=new URL(req.url,'http://localhost').pathname;
 if(name==='/forward_rules')name='/pages/forward_rules.html';
 if(/^\/admin\/users\/\d+\/forward_rules$/.test(name))name='/pages/admin_user_rules.html';
 const file=path.resolve(root,'.'+name);
 if(!file.startsWith(root+path.sep)||!fs.existsSync(file)||!fs.statSync(file).isFile()){res.writeHead(404);res.end();return}
 const types={'.html':'text/html; charset=utf-8','.js':'text/javascript; charset=utf-8','.css':'text/css','.svg':'image/svg+xml'};
 res.setHeader('Content-Type',types[path.extname(file)]||'application/octet-stream');fs.createReadStream(file).pipe(res);
});
const profile={id:2,username:'fixture@example.test',plan_id:1,plan_name:'fixture',plan_enabled:true,node_ids:[10],quota_bytes:-1,traffic_bytes:0,max_rules:0,max_connections:0,rule_speed_mbps:20,rule_ip_limit:0,rule_connection_limit:0,rule_count:2,expires_at:null};
const nodes=[{id:10,name:'入口一',ingress_enabled:true,allow_direct:true,tunnel_exit_enabled:false,tunnel_protocol:'plain_tcp',allowed_ingress_ids:[],public_address:'entry.example.test',port_min:1024,port_max:65535}];
function fixtures(){return [1,2].map(id=>({id,user_id:2,node_id:10,node_name:'入口一',egress_node_id:0,egress_node_name:'',protocol:'tcp',name:'rule-'+id,listen_port:id===2?443:20001,targets:['target.example.test:443'],group_id:0,group_name:'',balance:'random',proxy_accept:'off',proxy_send:'off',proxy_trusted_cidrs:[],enabled:true,public_address:'entry.example.test',traffic_bytes:0,status:'active',tls_inbound_id:id===2?7:0,sni_names:id===2?['a.example.test']:[],tls_alpn:[],origin_tls:{enabled:false},sync_error:'',configured_speed_mbps:0,speed_mbps:20,ip_limit:0,connection_limit:0}));}
(async()=>{
 await new Promise(resolve=>server.listen(0,'127.0.0.1',resolve));
 const base='http://127.0.0.1:'+server.address().port;
 const browser=await chromium.launch({headless:true,...(process.env.NEKOPASS_BROWSER?{executablePath:process.env.NEKOPASS_BROWSER}:{})});
 try{
  for(const managed of [false,true]){
   const rules=fixtures(),calls=[],errors=[];
   const context=await browser.newContext({viewport:{width:1440,height:1000}});const page=await context.newPage();
   page.setDefaultTimeout(15000);page.on('pageerror',e=>errors.push(e.message));
   await page.route('**/api/v1/**',async route=>{
    const req=route.request(),api=new URL(req.url()).pathname.slice('/api/v1/'.length);
    if(req.method()==='PUT'&&/rules\/\d+$/.test(api)){
     const body=req.postDataJSON(),id=Number(api.split('/').at(-1));calls.push({api,body});
     Object.assign(rules.find(r=>r.id===id),body,{id,configured_speed_mbps:body.speed_mbps,speed_mbps:body.speed_mbps?Math.min(20,body.speed_mbps):20});
     return route.fulfill({json:{ok:true}});
    }
    let data={};if(api==='me')data={id:managed?1:2,username:managed?'admin':'fixture@example.test',is_admin:managed};
    else if(api==='profile'||api==='admin/users/2')data=[profile];
    else if(api==='rules'||api==='admin/users/2/rules')data=rules;
    else if(api==='rule-nodes'||api==='admin/users/2/rule-nodes')data=nodes;
    else if(api.endsWith('tls-inbounds'))data=[{id:7,node_id:10,name:'共享入口',port:443,allowed_sni:[]}];
    else if(api.endsWith('rule-egress-groups')||api.endsWith('rule-ingress-groups')||api.endsWith('rule-groups'))data=[];
    else if(api==='site')data={name:'Nekopass',site_name:'Nekopass'};
    await route.fulfill({json:data});
   });
   await page.goto(base+(managed?'/admin/users/2/forward_rules':'/forward_rules'));
   const rows=page.locator('.rules-table .el-table__body-wrapper tbody tr');await rows.first().waitFor();
   async function edit(index){await rows.nth(index).getByRole('button',{name:'编辑规则',exact:true}).click();const dlg=page.getByRole('dialog',{name:'编辑规则',exact:true});await dlg.waitFor();return dlg}
   const speed=dlg=>dlg.locator('.el-form-item').filter({hasText:'本规则双向合计限速'}).locator('input');
   async function save(dlg,value){await speed(dlg).fill(String(value));await speed(dlg).blur();await dlg.getByRole('button',{name:'确定',exact:true}).click();await dlg.waitFor({state:'hidden'});await page.waitForTimeout(100)}
   for(const index of [0,1]){
    let dlg=await edit(index);assert.equal(await speed(dlg).inputValue(),'0');
    await save(dlg,10);assert.equal(calls.at(-1).body.speed_mbps,10);assert.equal(calls.at(-1).api,`${managed?'admin/users/2/':''}rules/${index+1}`);
    dlg=await edit(index);assert.equal(await speed(dlg).inputValue(),'10');
    await save(dlg,0);assert.equal(calls.at(-1).body.speed_mbps,0);
    dlg=await edit(index);assert.equal(await speed(dlg).inputValue(),'0');await dlg.getByRole('button',{name:'取消',exact:true}).click();
   }
   assert.deepEqual(errors,[]);console.log('PASS rule speed normal/shared TLS, set/reload/clear; managed='+managed);await context.close();
  }
 }finally{await browser.close();await new Promise(resolve=>server.close(resolve))}
})().catch(err=>{console.error(err);server.close();process.exitCode=1});
