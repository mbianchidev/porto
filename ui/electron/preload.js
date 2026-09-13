const { contextBridge, ipcRenderer } = require('electron')

contextBridge.exposeInMainWorld('portoDesktop', Object.freeze({
  getPreferences: () => ipcRenderer.invoke('porto:desktop-preferences:get'),
  setPreferences: (preferences) => ipcRenderer.invoke('porto:desktop-preferences:set', preferences),
  getUpdateStatus: () => ipcRenderer.invoke('porto:updates:get-status'),
  checkForUpdates: () => ipcRenderer.invoke('porto:updates:check'),
  downloadUpdate: () => ipcRenderer.invoke('porto:updates:download'),
  restartAndInstallUpdate: () => ipcRenderer.invoke('porto:updates:restart-and-install'),
  onUpdateStatus: (listener) => {
    if (typeof listener !== 'function') throw new Error('Update status listener must be a function')
    const handler = (_event, status) => listener(status)
    ipcRenderer.on('porto:updates:status', handler)
    return () => ipcRenderer.removeListener('porto:updates:status', handler)
  },
}))
