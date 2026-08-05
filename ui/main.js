// RTSP Streaming Service - Electron 主进程
// 职责：引擎进程生命周期管理（启动/守护/退出）、日志转发、窗口管理
const { app, BrowserWindow, ipcMain, clipboard, session } = require('electron')
const { spawn } = require('child_process')
const path = require('path')
const fs = require('fs')

const ENGINE_HTTP = process.env.RTSP_ENGINE_HTTP || '127.0.0.1:18080' // 默认避开 Windows 保留的 808x 段
const DEFAULT_RTSP = ':8554'
const MAX_CONSECUTIVE_FAILS = 3 // 连续快速失败次数上限，超过则停止自动重启并提示
const STABLE_MS = 5000 // 引擎稳定运行阈值（超过即视为启动成功，重置失败计数）
let mainWindow = null
let engineProc = null
let engineRunning = false
let engineInfo = null // 引擎实际监听地址（端口被占自动顺延时由引擎上报）
let failCount = 0
let stableTimer = null
let restartTimer = null

// ---------- 引擎路径解析 ----------
function engineBinPath() {
  const exe = process.platform === 'win32' ? 'rtsp-engine.exe' : 'rtsp-engine'
  // 1) 打包模式：resources/engine/rtsp-engine.exe
  if (app.isPackaged) {
    const p = path.join(process.resourcesPath, 'engine', exe)
    if (fs.existsSync(p)) return p
  }
  // 2) 开发模式：环境变量优先
  if (process.env.RTSP_ENGINE) return process.env.RTSP_ENGINE
  // 3) 开发模式：../engine/bin/rtsp-engine
  return path.join(__dirname, '..', 'engine', 'bin', exe)
}

function engineConfigPath() {
  const dir = app.getPath('userData')
  return path.join(dir, 'engine-config.yaml')
}

// 工厂默认值（与 ensureConfig 首次生成的内容一致，供设置页"恢复默认"使用）
function factoryDefaults() {
  const ffmpegBin = path.join(process.resourcesPath, 'engine', 'ffmpeg.exe')
  return {
    http_listen: ENGINE_HTTP,
    rtsp_listen: DEFAULT_RTSP,
    ffmpeg_path: (app.isPackaged && fs.existsSync(ffmpegBin)) ? ffmpegBin : 'ffmpeg',
    log_level: 'info',
    auth: { username: '', password: '' },
  }
}

// ---------- 引擎进程管理 ----------
function ensureConfig() {
  const cfgPath = engineConfigPath()
  // FFmpeg 路径：打包后指向 resources/engine/ffmpeg.exe，开发模式走 PATH
  const ffmpegBin = path.join(process.resourcesPath, 'engine', 'ffmpeg.exe')
  const ffmpegPath = (app.isPackaged && fs.existsSync(ffmpegBin)) ? ffmpegBin : 'ffmpeg'
  const dataDir = path.join(app.getPath('userData'), 'data').replace(/\\/g, '\\\\')

  let existing = null
  if (fs.existsSync(cfgPath)) {
    try { existing = fs.readFileSync(cfgPath, 'utf8') } catch {}
  }

  if (!existing) {
    // 首次启动：生成默认配置
    const def = factoryDefaults()
    const cfg = `# RTSP Streaming Service engine config (auto-generated)
# 以下为默认参数，开箱即用；如需自定义请在界面"设置"中修改
http_listen: ${def.http_listen}
rtsp_listen: ${def.rtsp_listen}
ffmpeg_path: ${ffmpegPath}
log_level: ${def.log_level}
data_dir: ${dataDir}
`
    fs.mkdirSync(path.dirname(cfgPath), { recursive: true })
    fs.writeFileSync(cfgPath, cfg, 'utf8')
    return cfgPath
  }

  // 配置已存在：每次启动刷新动态字段（ffmpeg_path / data_dir），
  // 避免软件目录移动或配置陈旧后指向旧位置
  const updated = existing
    .replace(/^ffmpeg_path:.*$/m, `ffmpeg_path: ${ffmpegPath}`)
    .replace(/^data_dir:.*$/m, `data_dir: ${dataDir}`)
  if (updated !== existing) {
    try {
      fs.writeFileSync(cfgPath, updated, 'utf8')
      console.log('[main] engine config refreshed (ffmpeg_path/data_dir follow current location)')
    } catch (err) {
      console.error('[main] refresh config failed:', err)
    }
  }
  return cfgPath
}

function startEngine() {
  const bin = engineBinPath()
  if (!fs.existsSync(bin)) {
    sendToUI('engine-status', { running: false, error: `engine binary not found: ${bin}` })
    return
  }
  const cfg = ensureConfig()
  console.log(`[main] starting engine: ${bin} --config ${cfg}`)
  engineProc = spawn(bin, ['--config', cfg], {
    stdio: ['pipe', 'pipe', 'pipe'],
    windowsHide: true,
  })
  engineRunning = true
  failCount = 0
  sendToUI('engine-status', { running: true, pid: engineProc.pid })

  // 引擎稳定运行超过阈值 → 视为启动成功，重置失败计数
  stableTimer = setTimeout(() => { failCount = 0 }, STABLE_MS)

  let stdoutBuf = ''
  const forward = (chunk) => {
    const text = chunk.toString()
    // 按完整行解析引擎上报行，避免管道块把行拆断导致正则匹配失败
    stdoutBuf += text
    let nlIdx
    while ((nlIdx = stdoutBuf.indexOf('\n')) >= 0) {
      const line = stdoutBuf.slice(0, nlIdx).trim()
      stdoutBuf = stdoutBuf.slice(nlIdx + 1)
      const m = line.match(/^RTSP_ENGINE_INFO http=(\S+) rtsp=(\S+)$/)
      if (m) {
        engineInfo = { http: m[1], rtsp: m[2] }
        sendToUI('engine-info', engineInfo)
      }
    }
    sendToUI('engine-log', text)
  }
  engineProc.stdout.on('data', forward)
  engineProc.stderr.on('data', forward)

  engineProc.on('exit', (code, signal) => {
    engineRunning = false
    clearTimeout(stableTimer)
    console.log(`[main] engine exited code=${code} signal=${signal}`)
    // 守护：非主动退出时自动重启（2s 延迟防崩溃循环）
    if (!app.isQuitting) {
      failCount++
      if (failCount >= MAX_CONSECUTIVE_FAILS) {
        // 连续多次快速失败 → 大概率配置问题（端口占用/FFmpeg 缺失），停止重启并引导用户
        const hint = '引擎连续启动失败，可能原因：端口被占用、FFmpeg 路径不正确。请点击右上角 ⚙ 设置检查，或先关闭占用端口的程序。'
        console.error('[main] ' + hint)
        sendToUI('engine-status', { running: false, code, error: hint })
        return
      }
      restartTimer = setTimeout(() => {
        if (!app.isQuitting) startEngine()
      }, 2000)
    }
  })
  engineProc.on('error', (err) => {
    engineRunning = false
    clearTimeout(stableTimer)
    console.error('[main] engine spawn error:', err)
    sendToUI('engine-status', { running: false, error: err.message })
  })
}

function stopEngine() {
  app.isQuitting = true
  if (engineProc && engineRunning) {
    // Windows 下先发 Ctrl+C 不可靠，直接 kill（引擎自身处理优雅退出）
    engineProc.kill()
  }
}

// ---------- 窗口 ----------
function createWindow() {
  mainWindow = new BrowserWindow({
    width: 1280,
    height: 800,
    title: 'RTSP Streaming Service',
    webPreferences: {
      preload: path.join(__dirname, 'preload.js'),
      contextIsolation: true,
      nodeIntegration: false,
    },
  })
  mainWindow.loadFile(path.join(__dirname, 'renderer', 'index.html'))
  mainWindow.on('closed', () => { mainWindow = null })
}

// ---------- IPC ----------
ipcMain.handle('engine:get-status', () => ({
  running: engineRunning,
  pid: engineProc?.pid ?? null,
  http: ENGINE_HTTP,
}))
ipcMain.handle('engine:get-info', () => engineInfo)
ipcMain.handle('engine:restart', () => {
  clearTimeout(restartTimer)
  failCount = 0 // 手动重启重置失败计数
  if (engineProc) engineProc.kill()
  setTimeout(startEngine, 500)
  return true
})
ipcMain.handle('engine:get-defaults', () => factoryDefaults())

// 复制文本到系统剪贴板（日志复制用）
ipcMain.handle('clipboard:write', (_e, text) => {
  clipboard.writeText(String(text ?? ''))
  return true
})

// 选择本地视频文件（文件源用）
ipcMain.handle('dialog:choose-video', async () => {
  const { dialog } = require('electron')
  const res = await dialog.showOpenDialog(mainWindow, {
    title: '选择视频文件',
    properties: ['openFile'],
    filters: [
      { name: '视频文件', extensions: ['mp4', 'avi', 'mkv', 'mov', 'wmv', 'flv', 'ts', 'm4v', 'webm'] },
      { name: '所有文件', extensions: ['*'] },
    ],
  })
  if (res.canceled || res.filePaths.length === 0) return null
  return res.filePaths[0]
})

function sendToUI(channel, payload) {
  if (mainWindow && !mainWindow.isDestroyed()) {
    mainWindow.webContents.send(channel, payload)
  }
}

// ---------- 生命周期 ----------
app.whenReady().then(() => {
  // 自动允许摄像头/麦克风权限（本机应用，摄像头用于采集推流；Windows 隐私总开关仍需用户开启）
  session.defaultSession.setPermissionRequestHandler((_wc, permission, callback) => {
    callback(permission === 'media' || permission === 'mediaKeySystem')
  })
  createWindow()
  startEngine()
  app.on('activate', () => {
    if (BrowserWindow.getAllWindows().length === 0) createWindow()
  })
})

app.on('window-all-closed', () => {
  if (process.platform !== 'darwin') app.quit()
})

app.on('before-quit', (e) => {
  stopEngine()
})
