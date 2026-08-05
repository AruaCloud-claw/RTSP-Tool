// preload：安全暴露主进程能力给渲染进程
const { contextBridge, ipcRenderer } = require('electron')

contextBridge.exposeInMainWorld('engineBridge', {
  getStatus: () => ipcRenderer.invoke('engine:get-status'),
  restart: () => ipcRenderer.invoke('engine:restart'),
  getDefaults: () => ipcRenderer.invoke('engine:get-defaults'),
  getInfo: () => ipcRenderer.invoke('engine:get-info'),
  writeClipboard: (text) => ipcRenderer.invoke('clipboard:write', text),
  chooseVideo: () => ipcRenderer.invoke('dialog:choose-video'),
  onStatus: (cb) => ipcRenderer.on('engine-status', (_e, p) => cb(p)),
  onInfo: (cb) => ipcRenderer.on('engine-info', (_e, p) => cb(p)),
  onLog: (cb) => ipcRenderer.on('engine-log', (_e, line) => cb(line)),
})
