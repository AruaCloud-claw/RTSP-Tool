// RTSP Streaming Service - 渲染进程逻辑
// API 地址默认指向 127.0.0.1:18080；引擎端口被占自动顺延时，由主进程上报实际地址后动态切换
let API = 'http://127.0.0.1:18080'
let WS_URL = 'ws://127.0.0.1:18080/ws'
let RTSP_BASE = '127.0.0.1:8554' // 内置 RTSP 服务展示地址（本地回环）

// 规范化引擎上报的监听地址："host:port"；若为 ":port"（无主机）则补 127.0.0.1
function normListen(addr) {
  if (!addr) return null
  const s = String(addr).trim()
  if (s.startsWith(':')) return '127.0.0.1' + s
  return s
}

function applyEngineInfo(info) {
  if (!info) return
  const http = normListen(info.http)
  if (http) {
    API = 'http://' + http
    WS_URL = 'ws://' + http + '/ws'
  }
  if (info.rtsp) {
    const port = String(info.rtsp).split(':').pop()
    if (port) RTSP_BASE = '127.0.0.1:' + port
  }
  appendLog(`[ui] 引擎地址: ${API} / RTSP ${RTSP_BASE}`)
  // 状态栏显示实际端口，用户可直观看到引擎在哪个端口
  engineText.textContent = `引擎运行中 ${http || ''}`
  engineText.style.color = ''
  // 服务页/拉流页地址卡片跟随
  const svcHttp = document.getElementById('svcHttp')
  const svcRtsp = document.getElementById('svcRtsp')
  if (svcHttp) svcHttp.textContent = API
  if (svcRtsp) svcRtsp.textContent = 'rtsp://' + RTSP_BASE
  // 第三方推流示例命令
  const pushCmd = `ffmpeg -re -i 视频源 -c:v copy -f rtsp -rtsp_transport tcp rtsp://${RTSP_BASE}/live/xxx`
  const svcPushCmd = document.getElementById('svcPushCmd')
  const extPushExample = document.getElementById('extPushExample')
  if (svcPushCmd) svcPushCmd.textContent = pushCmd
  if (extPushExample) extPushExample.textContent = pushCmd
}

// ---------- 弹窗统一开关（不依赖 CSS：同时用 class + 内联样式，保证任何情况都能关） ----------
function openModal(el) {
  el.classList.remove('hidden')
  el.style.display = 'flex'
}
function closeModal(el) {
  el.classList.add('hidden')
  el.style.display = 'none'
}
// 启动强制隐藏所有弹窗（直接进入主页面，不打扰用户）
document.querySelectorAll('.modal').forEach((m) => closeModal(m))

// 引擎状态显示
const engineLight = document.getElementById('engineLight')
const engineText = document.getElementById('engineText')

function setEngineStatus(running, error) {
  engineLight.className = 'light ' + (running ? 'on' : 'off')
  if (running) {
    engineText.textContent = '引擎运行中（默认配置）'
    engineText.style.color = ''
  } else if (error) {
    engineText.textContent = '引擎启动失败，点击左侧「设置」检查'
    engineText.style.color = '#e05a5a'
  } else {
    engineText.textContent = '引擎未运行'
    engineText.style.color = ''
  }
}

window.engineBridge?.onStatus((s) => {
  setEngineStatus(s.running, s.error)
  if (s.error) appendLog('[main] ' + s.error)
})
window.engineBridge?.onLog((line) => appendLog(line))
document.getElementById('btnRestart').addEventListener('click', () => {
  appendLog('[ui] 请求重启引擎...')
  window.engineBridge?.restart()
})

// 启动时查询一次状态
window.engineBridge?.getStatus().then((s) => setEngineStatus(s.running, s.error)).catch(() => {})
// 引擎上报实际监听地址（端口顺延后跟随）
window.engineBridge?.getInfo().then((info) => { if (info) { applyEngineInfo(info); loadStreams().catch(() => {}) } }).catch(() => {})
window.engineBridge?.onInfo((info) => {
  applyEngineInfo(info)
  reconnectWS() // 端口可能变化，重建 WS
  loadStreams().catch(() => {})
})
// ---------- 日志 ----------
const logArea = document.getElementById('logArea')
const MAX_LOG = 500
const chkAutoScroll = document.getElementById('chkAutoScroll')
// 自动滚动偏好持久化
if (chkAutoScroll) {
  try {
    chkAutoScroll.checked = localStorage.getItem('autoscroll') !== '0'
    chkAutoScroll.addEventListener('change', () => {
      localStorage.setItem('autoscroll', chkAutoScroll.checked ? '1' : '0')
      if (chkAutoScroll.checked) logArea.scrollTop = logArea.scrollHeight
    })
  } catch {}
}
function appendLog(line) {
  const text = String(line).trim()
  if (!text) return
  const div = document.createElement('div')
  div.textContent = text
  logArea.appendChild(div)
  while (logArea.childElementCount > MAX_LOG) logArea.removeChild(logArea.firstChild)
  if (!chkAutoScroll || chkAutoScroll.checked) logArea.scrollTop = logArea.scrollHeight
}
document.getElementById('btnClearLog').addEventListener('click', () => { logArea.innerHTML = '' })

// 复制全部日志到剪贴板
document.getElementById('btnCopyLog').addEventListener('click', async () => {
  const lines = [...logArea.children].map((d) => d.textContent).join('\n')
  if (!lines) {
    appendLog('[ui] 暂无日志可复制')
    return
  }
  try {
    if (window.engineBridge?.writeClipboard) {
      await window.engineBridge.writeClipboard(lines)
    } else {
      await navigator.clipboard.writeText(lines)
    }
    const btn = document.getElementById('btnCopyLog')
    const old = btn.textContent
    btn.textContent = '✓ 已复制'
    setTimeout(() => { btn.textContent = old }, 1500)
    appendLog(`[ui] 已复制 ${logArea.children.length} 条日志`)
  } catch (err) {
    appendLog('[ui] 复制失败: ' + err.message)
  }
})

// ---------- 新建流表单联动 ----------
const fSourceType = document.getElementById('fSourceType')
const argsFile = document.getElementById('argsFile')
const argsCamera = document.getElementById('argsCamera')
const argsRTSP = document.getElementById('argsRTSP')
const fTargetType = document.getElementById('fTargetType')
const argsTargetLocal = document.getElementById('argsTargetLocal')
const argsTargetRemote = document.getElementById('argsTargetRemote')

function syncForm() {
  const st = fSourceType.value
  argsFile.classList.toggle('hidden', st !== 'file')
  argsCamera.classList.toggle('hidden', st !== 'camera')
  argsRTSP.classList.toggle('hidden', st !== 'rtsp')
  const tt = fTargetType.value
  argsTargetLocal.classList.toggle('hidden', tt !== 'local')
  argsTargetRemote.classList.toggle('hidden', tt !== 'remote')
}
fSourceType.addEventListener('change', syncForm)
fTargetType.addEventListener('change', syncForm)
syncForm()

// 浏览文件按钮
if (window.engineBridge?.chooseVideo) {
  document.getElementById('btnBrowseFile').addEventListener('click', async () => {
    const path = await window.engineBridge.chooseVideo()
    if (path) document.getElementById('fFilePath').value = path
  })
}

// 检测摄像头设备：优先用浏览器内置 API（Windows 原生枚举，中文设备名无乱码），失败时回退引擎 API
const fDevice = document.getElementById('fDevice')
document.getElementById('btnScanCameras').addEventListener('click', async () => {
  fDevice.innerHTML = '<option value="">正在检测…</option>'
  try {
    const cams = await detectCameras()
    fDevice.innerHTML = ''
    if (!cams || cams.length === 0) {
      fDevice.innerHTML = '<option value="">未检测到设备（请确认摄像头已连接，且 Windows 设置→隐私→相机 允许应用使用）</option>'
      appendLog('[ui] 未检测到摄像头')
      return
    }
    for (const c of cams) {
      const opt = document.createElement('option')
      opt.value = c
      opt.textContent = c
      fDevice.appendChild(opt)
    }
    appendLog(`[ui] 检测到 ${cams.length} 个摄像头`)
  } catch (err) {
    fDevice.innerHTML = '<option value="">检测失败</option>'
    appendLog('[ui] 检测摄像头失败: ' + err.message)
  }
})

// 摄像头枚举：1) 浏览器原生（Windows Media Foundation） 2) 引擎 ffmpeg 回退
async function detectCameras() {
  try {
    // 先请求一次摄像头权限（解锁设备名显示，Windows 首次会弹授权）
    try {
      const stream = await navigator.mediaDevices.getUserMedia({ video: true })
      stream.getTracks().forEach((t) => t.stop())
    } catch {}
    const devices = await navigator.mediaDevices.enumerateDevices()
    const cams = devices.filter((d) => d.kind === 'videoinput' && d.label).map((d) => d.label)
    if (cams.length > 0) {
      appendLog('[ui] 摄像头枚举方式：系统原生')
      return cams
    }
    appendLog('[ui] 系统原生枚举无结果，回退引擎 API')
  } catch (err) {
    appendLog('[ui] 系统原生枚举不可用，回退引擎 API: ' + err.message)
  }
  const cams = await api('/api/v1/cameras')
  return Array.isArray(cams) ? cams : []
}

// ---------- 流 CRUD ----------
async function api(path, opts = {}) {
  const res = await fetch(API + path, {
    headers: { 'Content-Type': 'application/json' },
    ...opts,
  })
  if (!res.ok) {
    let msg = res.statusText
    try {
      const t = await res.text()
      if (t) {
        try { msg = JSON.parse(t).error || t } catch { msg = t }
      }
    } catch {}
    throw new Error(`${res.status} ${msg}`)
  }
  if (res.status === 204) return null
  return res.json()
}

// ---------- 流分类 ----------
// 推流：文件/摄像头源，或推送到外部服务器的流
function isPushStream(s) {
  return !(s.source_type === 'rtsp' && s.target_type === 'local')
}
// 拉流：RTSP 源 → 内置服务（级联）
function isPullStream(s) {
  return s.source_type === 'rtsp' && s.target_type === 'local'
}

async function loadStreams() {
  const list = await api('/api/v1/streams')
  renderStreams(list.filter(isPushStream)) // 推流页只显示推流
  syncPullPreviews(list) // 拉流页预览区跟随
}

function renderStreams(list) {
  document.getElementById('streamCount').textContent = list.length
  const tbody = document.getElementById('streamBody')
  if (!list.length) {
    tbody.innerHTML = '<tr class="empty"><td colspan="6">暂无流，去「推流」或「拉流」创建</td></tr>'
    return
  }
  tbody.innerHTML = ''
  for (const s of list) {
    const tr = document.createElement('tr')
    tr.dataset.id = s.id
    const srcDesc = describeSource(s)
    const tgtDesc = s.target_type === 'local'
      ? `内置 /${s.target_args?.path || ''}`
      : `外部 ${s.target_args?.url || ''}`
    const rtspUrl = s.target_type === 'local'
      ? `rtsp://${RTSP_BASE}/${s.target_args?.path || ''}`
      : '—'
    const statTxt = (s.status === 'running' || s.status === 'starting')
      ? `  ${Math.round(s.fps)}fps ${s.bitrate_kbps}kbps ${s.clients}客户端`
      : ''
    tr.innerHTML = `
      <td>${esc(s.name)} <span style="color:#666;font-size:11px">(${esc(s.id)})</span></td>
      <td>${esc(srcDesc)}</td>
      <td>${esc(tgtDesc)}</td>
      <td><span class="status ${s.status}">${statusText(s.status)}</span><span class="row-stats" style="color:#6a8f5a;font-size:11px">${esc(statTxt)}</span></td>
      <td class="url-cell">${esc(rtspUrl)}</td>
      <td><div class="row-actions">
        ${s.status === 'running' || s.status === 'starting'
          ? `<button class="btn tiny primary" data-act="preview" data-id="${esc(s.id)}">预览</button>
             <button class="btn tiny" data-act="stop" data-id="${esc(s.id)}">停止</button>`
          : `<button class="btn tiny primary" data-act="start" data-id="${esc(s.id)}">启动</button>`}
        <button class="btn tiny danger" data-act="del" data-id="${esc(s.id)}">删除</button>
      </div></td>`
    tbody.appendChild(tr)
  }
}

function describeSource(s) {
  const a = s.source_args || {}
  switch (s.source_type) {
    case 'file': return `文件 ${a.file_path}${a.loop ? ' (循环)' : ''}`
    case 'camera': return `摄像头 ${a.device}${a.width ? ` ${a.width}x${a.height}@${a.framerate}fps` : ''}`
    case 'rtsp': return `RTSP ${a.url}`
    default: return s.source_type
  }
}
function statusText(st) {
  return { running: '运行中', stopped: '已停止', starting: '启动中', error: '异常' }[st] || st
}
function esc(s) {
  return String(s ?? '').replace(/[&<>"']/g, (c) =>
    ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]))
}

// 操作按钮（事件委托）
document.getElementById('streamBody').addEventListener('click', async (e) => {
  const btn = e.target.closest('button[data-act]')
  if (!btn) return
  const { act, id } = btn.dataset
  try {
    if (act === 'preview') { previewStream(id, btn); return }
    if (act === 'snap') { takeSnapshot(id); return }
    if (act === 'start') { await api(`/api/v1/streams/${id}/start`, { method: 'POST' }); appendLog(`[ui] 已启动流 ${id}`) }
    if (act === 'stop') { await api(`/api/v1/streams/${id}/stop`, { method: 'POST' }); appendLog(`[ui] 已停止流 ${id}`) }
    if (act === 'del') {
      if (!confirm(`确认删除流 ${id}？`)) return
      await api(`/api/v1/streams/${id}`, { method: 'DELETE' })
      appendLog(`[ui] 已删除流 ${id}`)
    }
    await loadStreams()
  } catch (err) {
    appendLog(`[ui] 操作失败: ${err.message}`)
    alert('操作失败: ' + err.message)
  }
})

// ---------- 通用 WebRTC 连接（预览格子/弹窗共用） ----------
// 返回 PeerConnection；onState 可选回调（connectionstatechange）
async function connectWebRTC(id, videoEl, statusEl, offerPath, extra = {}, onState = null) {
  const pc = new RTCPeerConnection()
  if (onState) pc.addEventListener('connectionstatechange', () => onState(pc))
  pc.addTransceiver('video', { direction: 'recvonly' })

  const offer = await pc.createOffer()
  await pc.setLocalDescription(offer)
  await waitIceGathering(pc)

  const resp = await fetch(API + offerPath, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ sdp: pc.localDescription.sdp, ...extra }),
  })
  const data = await resp.json()
  if (!resp.ok) throw new Error(data.error || '信令失败')
  await pc.setRemoteDescription({ type: 'answer', sdp: data.sdp })

  videoEl.srcObject = new MediaStream(pc.getReceivers().map((r) => r.track))
  videoEl.play().catch(() => {})
  return pc
}

// ---------- 预览格子（多路预览） ----------
// 返回 {cell, video, status}；onClose 在点击 ✕ 时回调（用于清理管理表）
function addGridPreview(container, id, title, onClose) {
  const cell = document.createElement('div')
  cell.className = 'pv-cell'
  cell.dataset.key = id
  cell.innerHTML = `
    <video autoplay muted playsinline></video>
    <div class="pv-bar"><span class="pv-title"></span><span class="pv-status">连接中...</span>
      <button type="button" class="btn tiny">✕</button></div>`
  cell.querySelector('.pv-title').textContent = title
  const video = cell.querySelector('video')
  const status = cell.querySelector('.pv-status')
  cell.querySelector('button').addEventListener('click', () => {
    cell.remove()
    if (onClose) onClose()
  })
  container.prepend(cell)
  updateGridEmpty(container)
  return { cell, video, status }
}

function updateGridEmpty(container) {
  const empty = container.querySelector('.grid-empty')
  if (empty) empty.style.display = container.querySelectorAll('.pv-cell').length ? 'none' : ''
}

// 关闭某路预览：一律按 DOM data-key 定位删除（entry.cell 引用可能因重渲染失效），幂等
function closePreviewEntry(map, key) {
  const pv = map.get(key)
  if (pv && pv.pc) { try { pv.pc.close() } catch {} }
  for (const container of document.querySelectorAll('.preview-grid')) {
    const cell = container.querySelector(`.pv-cell[data-key="${CSS.escape(key)}"]`)
    if (cell) {
      cell.remove()
      updateGridEmpty(container)
    }
  }
  map.delete(key)
}

// 预览格子状态显示（连接中 → 实时 / 失败）
function gridStateHandler(statusEl) {
  return (pc) => {
    const st = pc.connectionState
    if (st === 'connected') {
      statusEl.textContent = '● 实时'
      statusEl.className = 'pv-status pv-live'
    } else if (st === 'failed') {
      statusEl.textContent = '连接失败'
      statusEl.className = 'pv-status'
    }
  }
}

// ---------- 拉流预览区同步 ----------
const pullPreviews = new Map() // streamId → {pc, cell}
function syncPullPreviews(streams) {
  const grid = document.getElementById('pullPreviewGrid')
  const pulls = streams.filter(isPullStream)
  document.getElementById('pullPreviewCount').textContent = pulls.filter((s) => s.status === 'running').length

  // 移除已停止/删除的拉流预览
  for (const id of [...pullPreviews.keys()]) {
    const s = pulls.find((x) => x.id === id)
    if (!s || s.status !== 'running') {
      const pv = pullPreviews.get(id)
      if (pv.pc) { try { pv.pc.close() } catch {} }
      pv.cell.remove()
      pullPreviews.delete(id)
    }
  }
  // 新增运行中的拉流预览
  for (const s of pulls) {
    if (s.status === 'running' && !grid.querySelector(`.pv-cell[data-key="${CSS.escape(s.id)}"]`)) {
      const cell = addGridPreview(grid, s.id, s.name, () => closePreviewEntry(pullPreviews, s.id))
      const entry = { pc: null, cell, container: grid }
      pullPreviews.set(s.id, entry)
      connectWebRTC(s.id, cell.video, cell.status, `/api/v1/streams/${s.id}/webrtc/offer`, {}, gridStateHandler(cell.status))
        .then((pc) => { const cur = pullPreviews.get(s.id); if (cur) cur.pc = pc })
        .catch((err) => { cell.status.textContent = '预览失败: ' + err.message })
    }
  }
  updateGridEmpty(grid)
}

// ---------- 外部推流（服务页） ----------
const extPreviews = new Map() // path → {pc, cell}
let extData = []

function renderExternal(list) {
  extData = Array.isArray(list) ? list : []
  document.getElementById('extCount').textContent = extData.length
  const tbody = document.getElementById('extBody')
  // 清理已消失路径的预览
  for (const p of [...extPreviews.keys()]) {
    if (!extData.find((e) => e.path === p)) closePreviewEntry(extPreviews, p)
  }
  if (!extData.length) {
    tbody.innerHTML = '<tr class="empty"><td colspan="4">暂无第三方推流</td></tr>'
    return
  }
  tbody.innerHTML = ''
  for (const e of extData) {
    const tr = document.createElement('tr')
    tr.innerHTML = `
      <td>${esc(e.path)}</td>
      <td class="url-cell">${esc(e.url)}</td>
      <td>${e.clients}</td>
      <td><div class="row-actions">
        <button type="button" class="btn tiny primary" data-act="ext-preview" data-path="${esc(e.path)}">预览</button>
        <button type="button" class="btn tiny" data-act="ext-close" data-path="${esc(e.path)}">关闭预览</button>
      </div></td>`
    tbody.appendChild(tr)
  }
}

async function loadExternal() {
  try {
    const list = await api('/api/v1/external-streams')
    renderExternal(list)
  } catch {}
}

document.getElementById('extBody').addEventListener('click', async (e) => {
  const btn = e.target.closest('button[data-act]')
  if (!btn) return
  const path = btn.dataset.path
  if (btn.dataset.act === 'ext-preview') {
    const grid = document.getElementById('extPreviewGrid')
    // 以 DOM 为准：格子在就不重复开（修复"关闭后无法重开"）
    if (grid.querySelector(`.pv-cell[data-key="${CSS.escape(path)}"]`)) return
    const cell = addGridPreview(grid, path, path, () => closePreviewEntry(extPreviews, path))
    const entry = { pc: null, cell, container: grid }
    extPreviews.set(path, entry)
    connectWebRTC(path, cell.video, cell.status, '/api/v1/webrtc/offer', { path }, gridStateHandler(cell.status))
      .then((pc) => { const cur = extPreviews.get(path); if (cur) cur.pc = pc })
      .catch((err) => { cell.status.textContent = '预览失败: ' + err.message })
  } else if (btn.dataset.act === 'ext-close') {
    closePreviewEntry(extPreviews, path)
  }
})

// ---------- WebRTC 预览 ----------
let previewPC = null

function waitIceGathering(pc) {
  return new Promise((resolve) => {
    if (pc.iceGatheringState === 'complete') return resolve()
    pc.addEventListener('icegatheringstatechange', () => {
      if (pc.iceGatheringState === 'complete') resolve()
    })
    setTimeout(resolve, 3000) // 兜底超时
  })
}

async function previewStream(id, btn) {
  const modal = document.getElementById('previewModal')
  const video = document.getElementById('previewVideo')
  const statusEl = document.getElementById('previewStatus')
  const title = document.getElementById('previewTitle')
  modal.classList.remove('hidden')
  statusEl.textContent = '正在建立 WebRTC 连接...'
  statusEl.className = 'preview-status'
  title.textContent = `预览流 ${id}`
  video.srcObject = null

  // 关闭上一次连接
  if (previewPC) { previewPC.close(); previewPC = null }

  try {
    const pc = await connectWebRTC(id, video, statusEl, `/api/v1/streams/${id}/webrtc/offer`, {}, (pc) => {
      const st = pc.connectionState
      if (st === 'connected') {
        statusEl.textContent = '● 实时预览中 (WebRTC)'
        statusEl.className = 'preview-status live'
      } else if (st === 'failed' || st === 'disconnected') {
        statusEl.textContent = '连接中断，正在重连...'
        statusEl.className = 'preview-status'
        setTimeout(() => { if (previewPC === pc) previewStream(id) }, 2000)
      }
    })
    previewPC = pc
    appendLog(`[ui] WebRTC 预览已连接: ${id}`)
  } catch (err) {
    statusEl.textContent = '预览失败: ' + err.message
    statusEl.className = 'preview-status'
    appendLog(`[ui] 预览失败: ${err.message}`)
    if (previewPC) { previewPC.close(); previewPC = null }
  }
}

document.getElementById('btnClosePreview').addEventListener('click', () => {
  document.getElementById('previewModal').classList.add('hidden')
  if (previewPC) { previewPC.close(); previewPC = null }
  document.getElementById('previewVideo').srcObject = null
})

// ---------- 快照 ----------
async function takeSnapshot(id) {
  const modal = document.getElementById('snapModal')
  const img = document.getElementById('snapImage')
  const status = document.getElementById('snapStatus')
  const title = document.getElementById('snapTitle')
  modal.classList.remove('hidden')
  title.textContent = `快照 - 流 ${id}`
  img.src = ''
  status.textContent = '正在抓取快照...'
  try {
    const res = await api(`/api/v1/streams/${id}/snapshot`, { method: 'POST' })
    img.src = API + res.url
    status.textContent = '已生成: ' + res.file
    appendLog(`[ui] 快照已生成: ${res.file}`)
  } catch (err) {
    status.textContent = '快照失败: ' + err.message
    appendLog(`[ui] 快照失败: ${err.message}`)
  }
}
document.getElementById('btnCloseSnap').addEventListener('click', () => {
  document.getElementById('snapModal').classList.add('hidden')
})

// ---------- WebSocket 实时监控 ----------
let wsConn = null
let wsRetryTimer = null

// 重建 WS（仅在需要切换地址时调用，避免误关新连接）
function reconnectWS() {
  if (wsConn) {
    const old = wsConn
    wsConn = null
    old.onclose = null
    try { old.close() } catch {}
  }
  if (wsRetryTimer) { clearTimeout(wsRetryTimer); wsRetryTimer = null }
  connectWS()
}

function connectWS() {
  // 已有连接（含连接中）则不重复创建
  if (wsConn && (wsConn.readyState === WebSocket.OPEN || wsConn.readyState === WebSocket.CONNECTING)) return
  try {
    const ws = new WebSocket(WS_URL)
    wsConn = ws
    ws.onmessage = (e) => {
      try {
        const msg = JSON.parse(e.data)
        if (msg.type === 'stats') {
          if (Array.isArray(msg.streams)) {
            updateStats(msg.streams)
            syncPullPreviews(msg.streams)
          }
          if (Array.isArray(msg.external)) renderExternal(msg.external)
        }
      } catch {}
    }
    ws.onclose = () => {
      if (wsConn === ws) {
        wsConn = null
        wsRetryTimer = setTimeout(connectWS, 3000) // 断线重连
      }
    }
    ws.onerror = () => { try { ws.close() } catch {} }
  } catch {}
}

// 用 WS 数据更新列表（替换轮询）
let statsCache = new Map()
function updateStats(streams) {
  statsCache = new Map(streams.map((s) => [s.id, s]))
  // 更新现有行的状态与统计
  for (const s of streams) {
    const row = document.querySelector(`#streamBody tr[data-id="${CSS.escape(s.id)}"]`)
    if (!row) continue
    const stCell = row.querySelector('.status')
    if (stCell) {
      stCell.className = 'status ' + s.status
      stCell.textContent = statusText(s.status)
    }
    const statSpan = row.querySelector('.row-stats')
    if (statSpan && (s.status === 'running' || s.status === 'starting')) {
      statSpan.textContent = `  ${s.fps.toFixed(0)}fps ${s.bitrate_kbps}kbps ${s.clients}客户端`
    }
  }
}

// ---------- 左侧导航 ----------
const navBtns = document.querySelectorAll('.nav-btn')
const pages = document.querySelectorAll('.page')
function showPage(name) {
  pages.forEach((p) => p.classList.toggle('hidden', p.id !== 'page-' + name))
  navBtns.forEach((b) => b.classList.toggle('active', b.dataset.page === name))
  if (name === 'settings') loadSettings()
}
navBtns.forEach((b) => b.addEventListener('click', () => showPage(b.dataset.page)))
showPage('services') // 启动默认进入主页面（服务）

// ---------- 设置 ----------
async function loadSettings() {
  const note = document.getElementById('settingsLoadNote')
  try {
    const cfg = await api('/api/v1/config')
    document.getElementById('sHttp').value = cfg.http_listen || ''
    document.getElementById('sRtsp').value = cfg.rtsp_listen || ''
    document.getElementById('sUser').value = cfg.auth?.username || ''
    document.getElementById('sPass').value = cfg.auth?.password || ''
    if (note) note.classList.add('hidden')
  } catch (err) {
    appendLog('[ui] 加载设置失败: ' + err.message)
    if (note) {
      note.textContent = '⚠ 无法读取当前配置（引擎未连接？）：' + err.message
      note.classList.remove('hidden')
    }
  }
}
// ESC 关闭所有弹窗 + 点击遮罩关闭
document.addEventListener('keydown', (e) => {
  if (e.key === 'Escape') {
    document.querySelectorAll('.modal:not(.hidden)').forEach((m) => closeModal(m))
  }
})
document.querySelectorAll('.modal').forEach((m) => {
  m.addEventListener('click', (e) => {
    if (e.target === m) closeModal(m) // 点击遮罩（弹窗外围）关闭
  })
})
// 恢复默认：用主进程提供的工厂默认值填回表单（FFmpeg 路径/日志级别由软件自动管理，不在此处暴露）
document.getElementById('btnResetSettings').addEventListener('click', async () => {
  try {
    const def = await window.engineBridge.getDefaults()
    document.getElementById('sHttp').value = def.http_listen || ''
    document.getElementById('sRtsp').value = def.rtsp_listen || ''
    document.getElementById('sUser').value = def.auth?.username || ''
    document.getElementById('sPass').value = def.auth?.password || ''
    appendLog('[ui] 已填入默认参数（需保存并重启引擎生效）')
  } catch (err) {
    appendLog('[ui] 获取默认参数失败: ' + err.message)
  }
})
document.getElementById('settingsForm').addEventListener('submit', async (e) => {
  e.preventDefault()
  const httpVal = document.getElementById('sHttp').value.trim()
  const rtspVal = document.getElementById('sRtsp').value.trim()
  // 输入校验：HTTP 监听必须 host:端口；RTSP 监听为 :端口 或 host:端口（拒绝双冒号等非法格式）
  if (httpVal && !/^[^:\s]+:\d{1,5}$/.test(httpVal)) {
    return alert('HTTP 监听格式应为 "host:端口"，例如 127.0.0.1:8080')
  }
  if (rtspVal && !/^(?:[^:\s]+:)?\d{1,5}$/.test(rtspVal)) {
    return alert('RTSP 监听格式应为 ":端口" 或 "host:端口"，例如 :8554')
  }
  const body = {
    http_listen: httpVal,
    rtsp_listen: rtspVal,
    auth: {
      username: document.getElementById('sUser').value,
      password: document.getElementById('sPass').value,
    },
  }
  try {
    const res = await api('/api/v1/config', { method: 'PUT', body: JSON.stringify(body) })
    appendLog('[ui] 设置已保存: ' + (res.note || ''))
    alert('设置已保存，重启引擎后生效')
  } catch (err) {
    appendLog('[ui] 保存设置失败: ' + err.message)
    alert('保存失败: ' + err.message)
  }
})

// 提交创建
document.getElementById('createForm').addEventListener('submit', async (e) => {
  e.preventDefault()
  const body = {
    name: document.getElementById('fName').value || 'unnamed',
    source_type: fSourceType.value,
    source_args: {},
    target_type: fTargetType.value,
    target_args: {},
  }
  const sa = body.source_args
  if (body.source_type === 'file') {
    sa.file_path = document.getElementById('fFilePath').value
    sa.loop = document.getElementById('fLoop').checked
    if (!sa.file_path) return alert('请填写文件路径')
  } else if (body.source_type === 'camera') {
    sa.device = document.getElementById('fDevice').value
    sa.width = +document.getElementById('fWidth').value || 0
    sa.height = +document.getElementById('fHeight').value || 0
    sa.framerate = +document.getElementById('fFps').value || 0
    if (!sa.device) return alert('请填写设备名')
  } else {
    sa.url = document.getElementById('fUrl').value
    if (!sa.url) return alert('请填写 RTSP 地址')
  }
  if (body.target_type === 'local') {
    body.target_args.path = document.getElementById('fPath').value || 'live/cam1'
  } else {
    body.target_args.url = document.getElementById('fTargetUrl').value
    if (!body.target_args.url) return alert('请填写外部服务器 URL')
  }
  try {
    await api('/api/v1/streams', { method: 'POST', body: JSON.stringify(body) })
    appendLog('[ui] 流创建成功')
    await loadStreams()
  } catch (err) {
    appendLog(`[ui] 创建失败: ${err.message}`)
    alert('创建失败: ' + err.message)
  }
})

// 拉流页提交：RTSP 源 → 内置服务（创建后自动启动，本机路径自动分配）
document.getElementById('pullForm').addEventListener('submit', async (e) => {
  e.preventDefault()
  const url = document.getElementById('pUrl').value.trim()
  if (!url) return alert('请填写外部 RTSP 地址')
  const body = {
    name: document.getElementById('pName').value.trim() || 'unnamed',
    source_type: 'rtsp',
    source_args: { url },
    target_type: 'local',
    target_args: { path: 'live/pull-' + Date.now().toString(36) },
  }
  try {
    await api('/api/v1/streams', { method: 'POST', body: JSON.stringify(body) })
    appendLog('[ui] 拉流创建成功')
    // 创建后自动启动
    const list = await api('/api/v1/streams')
    const created = [...list].reverse().find((s) => s.source_args?.url === url)
    if (created) {
      await api(`/api/v1/streams/${created.id}/start`, { method: 'POST' })
      appendLog(`[ui] 拉流已启动: ${created.id}`)
    }
    await loadStreams() // 预览区会自动出现该拉流
  } catch (err) {
    appendLog(`[ui] 创建拉流失败: ${err.message}`)
    alert('创建失败: ' + err.message)
  }
})

// 轮询刷新（P2 将换 WebSocket 实时推送）
setInterval(() => {
  if (document.visibilityState === 'visible') {
    loadStreams().catch(() => {})
    loadExternal().catch(() => {})
  }
}, 3000)

// WebSocket 实时监控（与轮询并存，WS 优先更新统计）
connectWS()
loadExternal().catch(() => {})

loadStreams().catch((err) => appendLog('[ui] 加载流列表失败: ' + err.message))
