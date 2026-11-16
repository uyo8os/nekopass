// Fixture APIs only; exercises actual browser drag, persistence and rollback.
const { chromium } = require('playwright');
const assert = require('node:assert/strict');
const http = require('node:http'), fs = require('node:fs'), path = require('node:path');
const root = path.resolve(__dirname, '../web/dist');
const server = http.createServer((req, res) => {
 let name = new URL(req.url, 'http://localhost').pathname;
 if (name === '/admin/nodes') name = '/pages/admin_nodes.html';
 if (name === '/admin/node_groups') name = '/pages/admin_node_groups.html';
 const file = path.resolve(root, '.' + name);
 if (!file.startsWith(root + path.sep) || !fs.existsSync(file) || !fs.statSync(file).isFile()) { res.writeHead(404); res.end(); return }
 res.setHeader('Content-Type', ({ '.html': 'text/html; charset=utf-8', '.js': 'text/javascript; charset=utf-8', '.css': 'text/css' })[path.extname(file)] || 'application/octet-stream');
 fs.createReadStream(file).pipe(res);
});

(async () => {
 await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
 const browser = await chromium.launch({ headless: true, ...(process.env.NEKOPASS_BROWSER ? { executablePath: process.env.NEKOPASS_BROWSER } : {}) });
 try {
  const node = (id, name, online) => ({ id, name, enabled: true, online, group_ids: [], ingress_enabled: true, tunnel_exit_enabled: false, tunnel_security: 'none', active_connections: 0, applied_revision: 1, config_revision: 1, sync_error: '', tls: {}, update_supported: false });
  let nodes = [node(1, '节点甲', false), node(2, '节点乙', true), node(3, '节点丙', false)];
  let groups = [11, 12, 13].map((id, i) => ({ id, name: ['组甲', '组乙', '组丙'][i], description: '', group_type: 'egress', enabled: true, sort_order: i, strategy: 'least_connections', node_ids: [], members: [], plan_count: 0 }));
  const writes = [], errors = [];
  let failNext = false, holdNextNodesGet = false, releaseStale, staleStarted, freshStarted;
  const page = await browser.newPage({ viewport: { width: 1600, height: 1000 } });
  page.setDefaultTimeout(10000);
  page.on('pageerror', error => errors.push(error.message));
  await page.route('**/api/v1/**', async route => {
   const req = route.request(), api = new URL(req.url()).pathname.slice('/api/v1/'.length);
   if (req.method() === 'PUT' && (api === 'admin/nodes/order' || api === 'admin/node-groups/order')) {
    const { ids } = req.postDataJSON();
    writes.push({ api, ids });
    if (failNext) { failNext = false; return route.fulfill({ status: 409, json: { error: '模拟排序失败' } }) }
    const source = api === 'admin/nodes/order' ? nodes : groups;
    assert.deepEqual([...ids].sort((a, b) => a - b), source.map(item => item.id).sort((a, b) => a - b));
    const sorted = ids.map(id => source.find(item => item.id === id));
    if (api === 'admin/nodes/order') nodes = sorted;
    else groups = sorted.map((item, i) => ({ ...item, sort_order: i }));
    return route.fulfill({ json: { ok: true } });
   }
   if (api === 'me') return route.fulfill({ json: { id: 1, username: 'fixture', is_admin: true } });
   if (api === 'admin/nodes') {
    if (holdNextNodesGet) {
     holdNextNodesGet = false;
     const stale = nodes.slice();
     await new Promise(resolve => { releaseStale = resolve; staleStarted() });
     return route.fulfill({ json: stale });
    }
    const result = await route.fulfill({ json: nodes });
    if (freshStarted) { freshStarted(); freshStarted = null }
    return result;
   }
   if (api === 'admin/node-groups') return route.fulfill({ json: groups });
   return route.fulfill({ json: { name: 'Nekopass' } });
  });
  const base = 'http://127.0.0.1:' + server.address().port;
  const handles = () => page.locator('.el-table__body-wrapper tbody button.sort-handle');
  async function expectOrder(ids) {
   await page.waitForFunction(expected => {
    const actual = Array.from(document.querySelectorAll('.el-table__body-wrapper tbody button.sort-handle'), e => Number(e.dataset.sortId));
    return JSON.stringify(actual) === JSON.stringify(expected);
   }, ids);
  }
  async function drag(id, targetID, after = true) {
   const source = page.locator(`button.sort-handle[data-sort-id="${id}"]`);
   const target = page.locator(`button.sort-handle[data-sort-id="${targetID}"]`).locator('xpath=ancestor::tr');
   const start = await source.boundingBox(), end = await target.boundingBox();
   assert.ok(start && end, 'drag source and target must be visible');
   await page.mouse.move(start.x + start.width / 2, start.y + start.height / 2);
   await page.mouse.down();
   await page.mouse.move(end.x + Math.min(80, end.width / 2), end.y + end.height * (after ? 0.75 : 0.25), { steps: 15 });
   await page.mouse.up();
  }
  async function dragAndWait(id, targetID, after = true) {
   await Promise.all([
    page.waitForResponse(response => response.request().method() === 'PUT' && /\/admin\/(nodes|node-groups)\/order$/.test(new URL(response.url()).pathname)),
    drag(id, targetID, after),
   ]);
  }
  async function bounded(promise, label) {
   let timeout;
   try { return await Promise.race([promise, new Promise((_, reject) => { timeout = setTimeout(() => reject(new Error(`Timed out waiting for ${label}`)), 12000) })]) }
   finally { clearTimeout(timeout) }
  }
  await page.goto(base + '/admin/nodes');
  await expectOrder([1, 2, 3]);
  await dragAndWait(1, 3);
  await expectOrder([2, 3, 1]);
  assert.deepEqual(writes.at(-1), { api: 'admin/nodes/order', ids: [2, 3, 1] });
  await page.reload(); await expectOrder([2, 3, 1]);

  failNext = true;
  await dragAndWait(1, 2, false);
  await page.getByText('排序保存失败：模拟排序失败', { exact: true }).waitFor();
  await expectOrder([2, 3, 1]);
  assert.deepEqual(nodes.map(n => n.id), [2, 3, 1]);

  // An earlier polling response may arrive after the save. It must not overwrite
  // the result or prevent a subsequent refresh from converging to server order.
  let staleReady;
  const staleRequest = new Promise(resolve => { staleReady = resolve });
  staleStarted = staleReady; holdNextNodesGet = true;
  await bounded(staleRequest, 'the earlier polling request');
  await dragAndWait(2, 1);
  await expectOrder([3, 1, 2]);
  assert.deepEqual(nodes.map(n => n.id), [3, 1, 2]);
  const freshRequest = new Promise(resolve => { freshStarted = resolve });
  await page.evaluate(() => {
   window.__observedOrders = [];
   const observer = new MutationObserver(() => {
    const ids = Array.from(document.querySelectorAll('.el-table__body-wrapper tbody button.sort-handle'), e => Number(e.dataset.sortId));
    if (ids.length === 3) window.__observedOrders.push(ids.join(','));
   });
   observer.observe(document.querySelector('.el-table__body-wrapper tbody'), { childList: true, subtree: true });
   window.__orderObserver = observer;
  });
  releaseStale();
  await bounded(freshRequest, 'the fresh request after sorting');
  await expectOrder([3, 1, 2]);
  const observed = await page.evaluate(() => { window.__orderObserver.disconnect(); return window.__observedOrders });
  assert.ok(!observed.includes('2,3,1'), `stale response must not revert the list: ${observed}`);
  await page.reload(); await expectOrder([3, 1, 2]);

  await page.goto(base + '/admin/nodes?status=offline');
  await expectOrder([3, 1]);
  assert.equal(await handles().count(), 2);
  assert.equal(await handles().nth(0).isDisabled(), true);
  assert.equal(await handles().nth(1).isDisabled(), true);
  const beforeFilter = writes.length;
  await drag(3, 1);
  await page.keyboard.press('ArrowDown');
  assert.equal(writes.length, beforeFilter);
  assert.deepEqual(nodes.map(n => n.id), [3, 1, 2]);

  await page.goto(base + '/admin/node_groups');
  await expectOrder([11, 12, 13]);
  await dragAndWait(11, 13);
  await expectOrder([12, 13, 11]);
  assert.deepEqual(writes.at(-1), { api: 'admin/node-groups/order', ids: [12, 13, 11] });
  await page.reload(); await expectOrder([12, 13, 11]);
  failNext = true;
  await dragAndWait(11, 12, false);
  await page.getByText('排序保存失败：模拟排序失败', { exact: true }).waitFor();
  await expectOrder([12, 13, 11]);
  assert.deepEqual(groups.map(g => g.id), [12, 13, 11]);

  await page.locator('button.sort-handle[data-sort-id="11"]').focus();
  await Promise.all([
   page.waitForResponse(response => response.request().method() === 'PUT' && new URL(response.url()).pathname.endsWith('/admin/node-groups/order')),
   page.keyboard.press('ArrowUp'),
  ]);
  await expectOrder([12, 11, 13]);
  assert.deepEqual(writes.at(-1), { api: 'admin/node-groups/order', ids: [12, 11, 13] });
  await page.reload(); await expectOrder([12, 11, 13]);
  assert.deepEqual(errors, []);
  console.log('PASS node and group mouse sorting, persistence, rollback, filtering and keyboard sorting');
 } finally {
  await browser.close();
  await new Promise(resolve => server.close(resolve));
 }
})().catch(error => { console.error(error); server.close(); process.exitCode = 1 });
