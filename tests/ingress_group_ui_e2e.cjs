// Local fixture APIs; build web first and provide Playwright on NODE_PATH.
const {chromium}=require('playwright');
const assert=require('node:assert/strict');
const http=require('node:http'),fs=require('node:fs'),path=require('node:path');
const root=path.resolve(__dirname,'../web/dist');
const server=http.createServer((req,res)=>{
 let name=new URL(req.url,'http://localhost').pathname;if(name==='/forward_rules')name='/pages/forward_rules.html';
 const file=path.resolve(root,'.'+name);if(!file.startsWith(root+path.sep)||!fs.existsSync(file)||!fs.statSync(file).isFile()){res.writeHead(404);res.end();return}
 res.setHeader('Content-Type',({'.html':'text/html; charset=utf-8','.js':'text/javascript; charset=utf-8','.css':'text/css'})[path.extname(file)]||'application/octet-stream');fs.createReadStream(file).pipe(res);
});
const nodes=[
 {id:10,name:'入口一',ingress_enabled:true,allow_direct:true,tunnel_exit_enabled:false,tunnel_protocol:'plain_tcp',allowed_ingress_ids:[],public_address:'one.example.test',port_min:80,port_max:9000},
 {id:11,name:'入口二',ingress_enabled:true,allow_direct:true,tunnel_exit_enabled:false,tunnel_protocol:'plain_tcp',allowed_ingress_ids:[],public_address:'two.example.test',port_min:443,port_max:10000},
];
(async()=>{
 await new Promise(resolve=>server.listen(0,'127.0.0.1',resolve));
 const browser=await chromium.launch({headless:true,...(process.env.NEKOPASS_BROWSER?{executablePath:process.env.NEKOPASS_BROWSER}:{})});
 try{
  const page=await browser.newPage({viewport:{width:1440,height:1100}});page.setDefaultTimeout(15000);
  const errors=[];page.on('pageerror',e=>errors.push(e.message));let saved,changed;
  await page.addInitScript(()=>Object.defineProperty(navigator,'clipboard',{value:{writeText:async text=>{window.__copied=text}},configurable:true}));
  await page.route('**/api/v1/**',async route=>{
   const req=route.request(),api=new URL(req.url()).pathname.slice('/api/v1/'.length);
   if(req.method()==='POST'&&api==='rules'){saved=req.postDataJSON();return route.fulfill({json:{id:1}})}
   if(req.method()==='PUT'&&api==='rules/1'){changed=req.postDataJSON();return route.fulfill({json:{id:1}})}
   let data=[];
   if(api==='me')data={id:2,username:'fixture',is_admin:false};
   else if(api==='site')data={name:'Nekopass'};
   else if(api==='profile')data=[{id:2,node_ids:[10],quota_bytes:-1,max_rules:0,traffic_bytes:0,rule_count:0,plan_id:1,plan_enabled:true}];
   else if(api==='rule-nodes')data=nodes.filter(n=>n.id===10);
   else if(api==='rule-ingress-groups')data=[{id:4,name:'多入口',node_ids:[10,11],members:nodes.map(n=>({...n,enabled:n.id===10,allow_direct:n.id===10,deny_udp_ingress_creation:n.id===11}))}];
   else if(api==='rules'&&saved)data=[{...saved,id:1,node_name:'入口一',ingress_group_name:'多入口',ingress_connection_address:'entry.example.test',ingress_nodes:nodes.map(n=>({id:n.id,name:n.name,public_address:n.public_address,enabled:true,online:true,applied_revision:1,config_revision:1,sync_error:''})),egress_node_name:'',public_address:'one.example.test',username:'fixture',traffic_bytes:0,status:'active',sync_error:'',group_name:''}];
   if(api==='rules'&&saved)data.push({...data[0],id:2,name:'旧入口组',ingress_connection_address:''});
   return route.fulfill({json:data});
  });
  const field=(dlg,label)=>dlg.getByText(label,{exact:true}).locator('..');
  await page.goto('http://127.0.0.1:'+server.address().port+'/forward_rules');
  await page.getByRole('button',{name:'添加规则',exact:true}).click();
  const dlg=page.getByRole('dialog',{name:'添加规则',exact:true});
  await field(dlg,'入口').locator('.el-select').click();await page.getByRole('option',{name:'多入口（2 个入口）',exact:true}).click();
  await dlg.getByText('规则将在组内入口使用相同端口',{exact:false}).waitFor();
  await dlg.getByText('限速、IP 数及连接数按各入口分别限制。',{exact:false}).waitFor();
  await field(dlg,'入站方式').locator('.el-select').click();assert.equal(await page.getByRole('option',{name:'共享 TLS 入站',exact:true}).getAttribute('aria-disabled'),'true');await page.getByRole('option',{name:'普通 TCP 端口',exact:true}).click();
  assert.match(await field(dlg,'监听端口').innerText(),/443–9000/);
  await field(dlg,'名称').locator('input').fill('组入口规则');await field(dlg,'监听端口').locator('input').fill('8080');await field(dlg,'目标地址').locator('textarea').fill('target.example.test:443');
  await dlg.getByRole('button',{name:'确定',exact:true}).click();await dlg.waitFor({state:'hidden'});
  assert.equal(saved.ingress_group_id,4);assert.equal(saved.listen_port,8080);assert.equal(saved.node_id,10);
  const row=page.locator('.rules-table .el-table__body-wrapper tbody tr').filter({hasText:'组入口规则'});
  await row.waitFor();assert.match(await row.innerText(),/entry\.example\.test:8080/);assert.doesNotMatch(await row.innerText(),/one\.example\.test:8080|two\.example\.test:8080|使用域名或外部负载均衡器分流/);
  await row.getByRole('button',{name:'entry.example.test:8080'}).click();assert.equal(await page.evaluate(()=>window.__copied),'entry.example.test:8080');
  const members=row.getByRole('button',{name:'查看入口节点'});await members.hover();
  const popover=page.locator('.el-popover:visible').filter({hasText:'入口节点'});await popover.waitFor();assert.match(await popover.innerText(),/one\.example\.test:8080/);assert.match(await popover.innerText(),/two\.example\.test:8080/);assert.match(await popover.innerText(),/已同步/);
  await page.mouse.move(0,0);await popover.waitFor({state:'hidden'});await members.focus();await popover.waitFor();assert.match(await popover.innerText(),/入口一/);await members.blur();await popover.waitFor({state:'hidden'});await members.click();await popover.waitFor();await page.mouse.click(0,0);
  const legacy=page.locator('.rules-table .el-table__body-wrapper tbody tr').filter({hasText:'旧入口组'});await legacy.waitFor();assert.match(await legacy.innerText(),/未配置入口组连接地址/);assert.equal(await legacy.getByRole('button',{name:'旧入口组'}).count(),0);
  await page.locator('.rule-dock').getByRole('button',{name:'导出',exact:true}).click();
  const exportDialog=page.getByRole('dialog',{name:'批量导出规则'});await exportDialog.waitFor();
  assert.equal(await exportDialog.getByRole('radio',{name:'Nekopass 完整备份'}).isChecked(),true);
  await exportDialog.getByText('入口组规则必须使用 Nekopass 完整备份，nyanpass 格式无法保留入口组。').waitFor();
  await exportDialog.getByText('nyanpass 兼容格式',{exact:true}).click();await exportDialog.getByRole('button',{name:'导出',exact:true}).click();
  await page.getByText('规则「组入口规则」使用入口节点组，请使用 Nekopass 完整备份保留入口组设置',{exact:true}).waitFor();
  await exportDialog.getByRole('button',{name:'取消',exact:true}).click();
  await row.getByRole('button',{name:'重命名',exact:true}).click();
  const renameDialog=page.getByRole('dialog',{name:'重命名规则'});await renameDialog.locator('input').fill('新名称');await renameDialog.getByRole('button',{name:'保存',exact:true}).click();
  await page.waitForFunction(()=>document.querySelectorAll('.el-message-box__wrapper').length===0);assert.equal(changed.ingress_group_id,4);assert.equal(changed.name,'新名称');
  await row.getByRole('button',{name:'复制规则',exact:true}).click();
  const copyDialog=page.getByRole('dialog',{name:'添加规则',exact:true});await copyDialog.waitFor();await copyDialog.getByRole('button',{name:'确定',exact:true}).click();await copyDialog.waitFor({state:'hidden'});
  assert.equal(saved.ingress_group_id,4);assert.equal(saved.name,'组入口规则 副本');
  assert.deepEqual(errors,[]);console.log('PASS ingress group creation, addresses, backups, copy and rename');
 }finally{await browser.close();await new Promise(resolve=>server.close(resolve))}
})().catch(err=>{console.error(err);server.close();process.exitCode=1});
