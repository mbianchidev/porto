const { contextBridge, ipcRenderer } = require('electron')

contextBridge.exposeInMainWorld('portoDesktop', Object.freeze({
  getPreferences: () => ipcRenderer.invoke('porto:desktop-preferences:get'),
  setPreferences: (preferences) => ipcRenderer.invoke('porto:desktop-preferences:set', preferences),
}))
