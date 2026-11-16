// Run after the frontend build. The API is stubbed to exercise only the diagnosis UI.
const { chromium } = require('playwright')
const assert = require('node:assert/strict')
const http = require('node:http'), fs = require('node:fs'), path = require('node:path')
const root = path.resolve(__dirname, '../web/dist')
const server = http.createServer((req, res) => {
  let name = new URL(req.url, 'http://localhost').pathname
  if (name === '/forward_rules') name = '/pages/forward_rules.html'
  const file = path.resolve(root, '.' + name)
  if (!file.startsWith(root + path.sep) || !fs.existsSync(file) || !fs.statSync(file).isFile()) { res.writeHead(404); res.end(); return }
  res.setHeader('Content-Type', ({ '.html': 'text/html; charset=utf-8', '.js': 'text/javascript; charset=utf-8', '.css': 'text/css' })[path.extname(file)] || 'application/octet-stream')
  fs.createReadStream(file).pipe(res)
})
const base = { user_id: 2, node_id: 10, node_name: '入口一', public_address: 'one.example.test', username: 'fixture', traffic_bytes: 0, status: 'active', sync_error: '', group_name: '', targets: ['target.example.test:443'], protocol: 'tcp', enabled: true, listen_port: 8080, proxy_trusted_cidrs: [] }
const direct = { ...base, id: 1, name: '直转规则' }
const relay = { ...base, id: 2, name: '中转规则', egress_node_id: 20, egress_node_name: '出口一' }
const ingress = { id: 'in:10', kind: 'ingress', name: '入口名称特别长但详情中应显示完整的名字', address: 'one.example.test' }
const target = { id: 'target:0', kind: 'target', name: '目标一', address: 'target.example.test:443' }
const exit = { id: 'out:20', kind: 'egress', name: '出口一', address: 'two.example.test' }
const directResult = { rule_id: 1, protocol: 'tcp', nodes: [ingress, target], links: [{ source: ingress.id, target: target.id, status: 'reachable', latency_ms: 48.2 }] }
const relayResult = { rule_id: 2, protocol: 'udp', nodes: [ingress, exit, target], links: [{ source: ingress.id, target: exit.id, status: 'unreachable', reason: '出口隧道不可达' }, { source: exit.id, target: target.id, status: 'unknown', reason: 'UDP 无法确认目标是否响应' }] }
;(async () => {
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve))
  const browser = await chromium.launch({ headless: true, ...(process.env.NEKOPASS_BROWSER ? { executablePath: process.env.NEKOPASS_BROWSER } : {}) })
  try {
    const page = await browser.newPage({ viewport: { width: 1440, height: 1000 } })
    const errors = [], calls = []
    page.on('pageerror', e => errors.push(e.message))
    let releaseRetry
    const retryGate = new Promise(resolve => { releaseRetry = resolve })
    await page.route('**/api/v1/**', async route => {
      const request = route.request(), api = new URL(request.url()).pathname.slice('/api/v1/'.length)
      if (api.endsWith('/diagnose')) {
        calls.push({ api, method: request.method() })
        if (api === 'rules/1/diagnose' && calls.filter(call => call.api === api).length === 2) await retryGate
        if (api === 'rules/1/diagnose' && calls.filter(call => call.api === api).length === 3) return route.fulfill({ status: 429, json: { error: '诊断过于频繁，请稍后再试' } })
        return route.fulfill({ json: api === 'rules/1/diagnose' ? directResult : relayResult })
      }
      let data = []
      if (api === 'me') data = { id: 2, username: 'fixture', is_admin: false }
      else if (api === 'site') data = { name: 'Nekopass' }
      else if (api === 'profile') data = [{ id: 2, node_ids: [10], quota_bytes: -1, max_rules: 0, traffic_bytes: 0, rule_count: 2, plan_id: 1, plan_enabled: true }]
      else if (api === 'rules') data = [direct, relay]
      return route.fulfill({ json: data })
    })
    await page.goto('http://127.0.0.1:' + server.address().port + '/forward_rules')
    const directRow = page.locator('.rules-table tbody tr').filter({ hasText: '直转规则' })
    await directRow.getByRole('button', { name: '规则诊断' }).click()
    const dialog = page.getByRole('dialog', { name: '规则诊断' })
    await dialog.locator('.diagnosis-label').getByText('48.2 ms', { exact: true }).waitFor()
    assert.equal(await dialog.locator('.diagnosis-column').count(), 2)
    assert.equal(await dialog.locator('.diagnosis-edge.reachable').count(), 1)
    await dialog.locator('summary').click()
    await dialog.getByText('入口名称特别长但详情中应显示完整的名字 → 目标一', { exact: true }).waitFor()
    assert.equal(await dialog.locator('.diagnosis-node-name').first().evaluate(el => el.getBBox().width <= 132), true, 'long names must fit in the node card')
    await page.evaluate(() => document.documentElement.classList.remove('dark'))
    const light = await dialog.locator('.diagnosis-scroll').evaluate(el => getComputedStyle(el).backgroundColor)
    await page.evaluate(() => document.documentElement.classList.add('dark'))
    const dark = await dialog.locator('.diagnosis-scroll').evaluate(el => getComputedStyle(el).backgroundColor)
    assert.notEqual(light, dark, 'diagnostic canvas should follow the theme')
    await dialog.getByRole('button', { name: '重新诊断' }).click()
    await dialog.getByText('正在重新诊断…').waitFor()
    assert.equal(await dialog.getByRole('button', { name: '重新诊断' }).isDisabled(), true)
    releaseRetry()
    await dialog.getByText('正在重新诊断…').waitFor({ state: 'hidden' })
    await dialog.getByRole('button', { name: '重新诊断' }).click()
    await dialog.getByText('诊断过于频繁，请稍后再试', { exact: true }).waitFor()
    assert.equal(await dialog.locator('.diagnosis-graph').count(), 0, 'failed retry must not display stale results')
    await dialog.getByRole('button', { name: '重新诊断' }).click()
    await dialog.locator('.diagnosis-label').getByText('48.2 ms', { exact: true }).waitFor()
    assert.equal(await dialog.getByText('诊断过于频繁，请稍后再试', { exact: true }).count(), 0)
    await dialog.getByRole('button', { name: '关闭', exact: true }).click()
    await page.locator('.rules-table tbody tr').filter({ hasText: '中转规则' }).getByRole('button', { name: '规则诊断' }).click()
    await dialog.locator('.diagnosis-edge.unreachable').waitFor()
    assert.equal(await dialog.locator('.diagnosis-column').count(), 3)
    assert.equal(await dialog.locator('.diagnosis-edge.unknown').count(), 1)
    await dialog.locator('summary').click()
    await dialog.getByText('出口隧道不可达', { exact: true }).waitFor()
    await dialog.getByText('UDP 无法确认目标是否响应', { exact: true }).waitFor()
    await dialog.getByText('UDP 无响应时无法确认可达性', { exact: false }).waitFor()
    if (process.env.NEKOPASS_SCREENSHOT) await page.screenshot({ path: process.env.NEKOPASS_SCREENSHOT })
    await page.setViewportSize({ width: 375, height: 800 })
    assert.equal(await dialog.locator('.diagnosis-scroll').evaluate(el => el.scrollWidth > el.clientWidth), true)
    assert.deepEqual(calls.map(c => c.method), ['POST', 'POST', 'POST', 'POST', 'POST'])
    assert.deepEqual(errors, [])
    console.log('PASS rule diagnosis topology, light/dark themes, retry, statuses and narrow screen')
  } finally { await browser.close(); await new Promise(resolve => server.close(resolve)) }
})().catch(err => { console.error(err); server.close(); process.exitCode = 1 })
