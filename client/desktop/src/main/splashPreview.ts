/**
 * Dev-only: visual preview of all splash screen states.
 *
 *   npm run preview:splash
 *
 * NOT imported by the production main process.
 * Cycles through: pulse → status updates → fill-progress → error → quit
 */

import path from 'node:path';
import { app, nativeImage } from 'electron';
import {
  showSplash,
  updateSplashStatus,
  showSplashProgress,
  updateSplashError,
  closeSplash,
} from './splashWindow';

app.whenReady().then(() => {
  const iconPath = path.join(process.cwd(), 'build', 'icon.png');
  const icon = nativeImage.createFromPath(iconPath);
  showSplash(icon.isEmpty() ? undefined : icon.toDataURL());

  // Sequence (ms offset → action)
  const steps: Array<[number, () => void]> = [
    // Pulse state — status copy from updater events
    [800, () => updateSplashStatus('Checking the airwaves...')],
    [1800, () => updateSplashStatus('New flight plan detected: v9.9.9')],

    // Fill-progress state — simulates download-progress events
    [
      2800,
      () => {
        showSplashProgress(0);
        updateSplashStatus('Fueling up... 0%');
      },
    ],
    [
      3300,
      () => {
        showSplashProgress(25);
        updateSplashStatus('Fueling up... 25%');
      },
    ],
    [
      3800,
      () => {
        showSplashProgress(50);
        updateSplashStatus('Fueling up... 50%');
      },
    ],
    [
      4300,
      () => {
        showSplashProgress(75);
        updateSplashStatus('Fueling up... 75%');
      },
    ],
    [
      4800,
      () => {
        showSplashProgress(100);
        updateSplashStatus('Ready for liftoff');
      },
    ],

    // Error state — simulates update-error or rollback detection
    [6200, () => updateSplashError('Houston, we have a problem')],

    // Done
    [
      7800,
      () => {
        closeSplash();
        app.quit();
      },
    ],
  ];

  for (const [delay, fn] of steps) {
    setTimeout(fn, delay);
  }
});
