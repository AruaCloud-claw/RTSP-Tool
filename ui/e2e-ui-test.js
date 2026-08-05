// e2e-ui-test.js — 在真实 Electron/Chromium 里验证服务页外部预览
const { app, BrowserWindow } = require('electron')
const path = require('path')

const UI = path.join(__dirname, 'renderer', 'index.html')
const results = { console: [], steps: [] }
let win = null

function log(msg) { console.log('[TEST]', msg); results.steps.push(msg) }

app.whenReady().then(async () => {
  win = new BrowserWindow({
    width: 1280, height: 800, show: false,
    webPreferences: { contextIsolation: false, nodeIntegration: true },
  })
  win.webContents.on('console-message', (_e, level, message) => {
    results.console.push(`[${level}] ${message}`)
  })
  await win.loadFile(UI)
  await win.webContents.executeJavaScript(`
    window.__errs = [];
    window.onerror = (m) => window.__errs.push('ERR:' + m);
    window.addEventListener('unhandledrejection', (e) => window.__errs.push('REJ:' + (e.reason && e.reason.message)));
    'ok'
  `)
  await new Promise((r) => setTimeout(r, 3000)) // 等初始加载 + loadExternal

  try {
    // 1. 切到服务页
    await win.webContents.executeJavaScript(`showPage('services'); 'ok'`)
    await new Promise((r) => setTimeout(r, 2500))

    // 2. 读取外部推流列表状态
    const listState = await win.webContents.executeJavaScript(`
      (() => {
        const rows = [...document.querySelectorAll('#extBody tr')].map(tr => tr.textContent.trim().slice(0, 60))
        return { rows, count: document.getElementById('extCount')?.textContent }
      })()
    `)
    log('外部推流列表: ' + JSON.stringify(listState))

    // 3. 模拟点击"预览"按钮
    const clicked = await win.webContents.executeJavaScript(`
      (() => {
        const btn = document.querySelector('#extBody button[data-act="ext-preview"]')
        if (!btn) return 'no-button'
        btn.click()
        return 'clicked:' + btn.dataset.path
      })()
    `)
    log('点击预览: ' + clicked)
    await new Promise((r) => setTimeout(r, 6000))

    // 3.4 初次预览深探针：getStats 看浏览器实际收到的字节 + video 状态
    const deepProbe = await win.webContents.executeJavaScript(`
      (async () => {
        const cell = document.querySelector('#extPreviewGrid .pv-cell[data-key="live/ext1"]')
        if (!cell) return 'no-cell'
        const v = cell.querySelector('video')
        const entry = extPreviews.get('live/ext1')
        let stats = {}
        if (entry && entry.pc) {
          const s = await entry.pc.getStats()
          s.forEach(r => {
            if (r.type === 'inbound-rtp') {
              stats.bytes = r.bytesReceived
              stats.packets = r.packetsReceived
              stats.decoded = r.framesDecoded
              stats.keyFrames = r.keyFramesDecoded
              stats.fir = r.firCount
              stats.pli = r.pliCount
              stats.nack = r.nackCount
              stats.lost = r.packetsLost
              stats.decoder = r.decoderImplementation
              stats.pts = r.lastPacketReceivedTimestamp
            }
          })
        }
        const track = v?.srcObject?.getVideoTracks?.()[0]
        return JSON.stringify({
          status: cell.querySelector('.pv-status')?.textContent,
          ready: v?.readyState, w: v?.videoWidth, h: v?.videoHeight,
          connState: entry?.pc?.connectionState,
          trackMuted: track?.muted, trackState: track?.readyState,
          stats,
        })
      })()
    `)
    log('初次预览探针: ' + deepProbe)
    await new Promise((r) => setTimeout(r, 3000))
    const deepProbe2 = await win.webContents.executeJavaScript(`
      (async () => {
        const entry = extPreviews.get('live/ext1')
        let stats = {}
        if (entry && entry.pc) {
          const s = await entry.pc.getStats()
          s.forEach(r => { if (r.type === 'inbound-rtp') {
            stats.bytes = r.bytesReceived; stats.packets = r.packetsReceived
            stats.decoded = r.framesDecoded
          }})
        }
        const v = document.querySelector('#extPreviewGrid .pv-cell video')
        return JSON.stringify({ ready: v?.readyState, w: v?.videoWidth, stats })
      })()
    `)
    log('3秒后探针: ' + deepProbe2)

    // 3.5 对比：直接调用 closePreviewEntry 验证关闭逻辑（含引用级探针）
    const closeDirect = await win.webContents.executeJavaScript(`
      (async () => {
        const domCell = document.querySelector('#extPreviewGrid .pv-cell[data-key="live/ext1"]')
        const entry = extPreviews.get('live/ext1')
        const refInfo = {
          domExists: !!domCell,
          entryExists: !!entry,
          sameNode: entry && domCell ? entry.cell === domCell : null,
          entryCellConnected: entry ? entry.cell.isConnected : null,
        }
        closePreviewEntry(extPreviews, 'live/ext1')
        await new Promise(r => setTimeout(r, 200))
        return JSON.stringify({
          ...refInfo,
          after: document.querySelectorAll('#extPreviewGrid .pv-cell').length,
          mapSize: extPreviews.size,
        })
      })()
    `)
    log('关闭探针: ' + closeDirect)

    // 3.6 对比：创建内部流（文件源）并预览，看 videoReady
    const internalTest = await win.webContents.executeJavaScript(`
      (async () => {
        try {
          const api = window.__API || 'http://127.0.0.1:18080'
          const r = await fetch(api + '/api/v1/streams', {
            method: 'POST', headers: {'Content-Type': 'application/json'},
            body: JSON.stringify({name:'int-test', source_type:'file',
              source_args:{file_path:'/tmp/ref.mp4', loop:true},
              target_type:'local', target_args:{path:'live/int-test'}})
          })
          const st = await r.json()
          await fetch(api + '/api/v1/streams/' + st.id + '/start', {method:'POST'})
          await new Promise(r => setTimeout(r, 2000))
          // 用弹窗预览内部流
          await previewStream(st.id)
          await new Promise(r => setTimeout(r, 6000))
          const v = document.getElementById('previewVideo')
          const track = v?.srcObject?.getVideoTracks?.()[0]
          const stt = document.getElementById('previewStatus')?.textContent
          return JSON.stringify({
            id: st.id,
            status: stt,
            ready: v?.readyState,
            w: v?.videoWidth, h: v?.videoHeight,
            trackState: track?.readyState,
          })
        } catch (e) { return 'ERR:' + e.message }
      })()
    `)
    log('内部流预览对比: ' + internalTest)

    // 4. 检查预览格子状态（含深探针）
    const gridState = await win.webContents.executeJavaScript(`
      (() => {
        const cells = [...document.querySelectorAll('#extPreviewGrid .pv-cell')].map(c => {
          const v = c.querySelector('video')
          const track = v?.srcObject?.getVideoTracks?.()[0]
          return {
            key: c.dataset.key,
            status: c.querySelector('.pv-status')?.textContent,
            hasVideo: !!v?.srcObject,
            videoReady: v?.readyState,
            videoW: v?.videoWidth,
            videoH: v?.videoHeight,
            trackState: track?.readyState,
            trackMuted: track?.muted,
            receivers: window.__pc ? 'n/a' : 'n/a',
          }
        })
        return { cells, errs: window.__errs }
      })()
    `)
    log('预览格子(深探针): ' + JSON.stringify(gridState))

    // 4.5 验证 sprop 注入后浏览器解码器
    const sdpInfo = await win.webContents.executeJavaScript(`
      (() => {
        // 尝试再次发起一次原生 RTCPeerConnection 直连检查 answer 是否含 sprop
        return 'skip'
      })()
    `)
    log('sdp 检查: ' + sdpInfo)

    // 5. 测试"关闭预览"（点击后立即 + 延迟检查）
    const closeResult = await win.webContents.executeJavaScript(`
      (async () => {
        const btn = document.querySelector('#extBody button[data-act="ext-close"]')
        if (!btn) return 'no-button'
        btn.click()
        await new Promise(r => setTimeout(r, 300))
        return JSON.stringify({
          cellsAfter300ms: document.querySelectorAll('#extPreviewGrid .pv-cell').length,
          mapSize: extPreviews.size,
          errs: window.__errs,
        })
      })()
    `)
    log('关闭预览(300ms后): ' + closeResult)
    await new Promise((r) => setTimeout(r, 1500))
    const afterClose = await win.webContents.executeJavaScript(`
      document.querySelectorAll('#extPreviewGrid .pv-cell').length
    `)
    log('关闭预览后格子数(1.8s): ' + afterClose)

    // 6. 测试重新打开
    await win.webContents.executeJavaScript(`
      (() => {
        const btn = document.querySelector('#extBody button[data-act="ext-preview"]')
        if (!btn) return 'no-button'
        btn.click()
        return 'clicked'
      })()
    `)
    await new Promise((r) => setTimeout(r, 5000))
    const reopened = await win.webContents.executeJavaScript(`
      [...document.querySelectorAll('#extPreviewGrid .pv-cell')].map(c => ({
        key: c.dataset.key,
        status: c.querySelector('.pv-status')?.textContent,
      }))
    `)
    log('重新打开后: ' + JSON.stringify(reopened))
  } catch (err) {
    log('测试异常: ' + err.message)
  }

  log('--- 控制台输出 ---')
  results.console.slice(0, 30).forEach((c) => console.log('[CONSOLE]', c))
  console.log('=== TEST DONE ===')
  app.exit(0)
})
