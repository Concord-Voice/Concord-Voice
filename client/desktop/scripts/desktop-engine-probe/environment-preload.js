'use strict';

// Electron supplies process to the preload function, not to world 999 globals.
// Report only these observed flags; no Node object or privileged API crosses.
const { contextBridge } = require('electron');
contextBridge.exposeInMainWorld(
  'engineProbeEnvironment',
  Object.freeze({
    sandboxed: process.sandboxed,
    contextIsolated: process.contextIsolated,
    type: process.type,
  })
);
