import React from 'react';
import { fireEvent, render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { NoiseGateCalibration } from '@/renderer/components/Settings/NoiseGateCalibration';
import type {
  GateCalibrationController,
  GateCalibrationState,
} from '@/renderer/hooks/device/useGateCalibration';
import type { UseMicTestReturn } from '@/renderer/hooks/device/useMicTest';
import type { CalibrationResult } from '@/renderer/services/voice/gateCalibration';
import { resetAllStores } from '../../../helpers/store-helpers';

const summary = {
  noiseP95Dbfs: -63,
  speechP10Dbfs: -38,
  speechP90Dbfs: -24,
  quietZeroFraction: 0,
  speechZeroFraction: 0.1,
};
const recommendation: CalibrationResult = {
  kind: 'recommended',
  thresholdDbfs: -57,
  summary,
};
const overlap: CalibrationResult = { kind: 'refused', reason: 'overlap', summary };

function fakeController(state: Partial<GateCalibrationState> = {}): GateCalibrationController {
  return {
    state: {
      phase: 'preparation',
      secondsRemaining: 10,
      meterDbfs: null,
      result: null,
      message: null,
      canStart: true,
      canFinishReading: false,
      ...state,
    },
    start: vi.fn(async () => {}),
    startReading: vi.fn(),
    finishReading: vi.fn(),
    cancel: vi.fn(),
    useThreshold: vi.fn(),
  };
}

function fakeMicTest(state: Partial<UseMicTestReturn> = {}): UseMicTestReturn {
  return {
    isTesting: false,
    isPending: false,
    dbfsLevel: -Infinity,
    inputOverloaded: false,
    dynamicGateStatus: null,
    error: null,
    startTest: vi.fn(async () => {}),
    stopTest: vi.fn(),
    startCalibration: vi.fn(async () => null),
    getActivityVersion: () => 0,
    ...state,
  };
}

type Props = React.ComponentProps<typeof NoiseGateCalibration>;
function setup(overrides: Partial<Props> = {}) {
  const props: Props = {
    controller: fakeController(),
    micTest: fakeMicTest(),
    mode: 'autoCalibrate',
    threshold: -50,
    appliedThreshold: -50,
    onThresholdChange: vi.fn(),
    ...overrides,
  };
  const view = render(<NoiseGateCalibration {...props} />);
  return {
    ...view,
    props,
    user: userEvent.setup(),
    update(next: Partial<Props>) {
      Object.assign(props, next);
      view.rerender(<NoiseGateCalibration {...props} />);
    },
  };
}

beforeEach(() => {
  resetAllStores();
});

describe('NoiseGateCalibration preparation and controls', () => {
  it('explains preparation before the user explicitly starts capture', async () => {
    const { props, user } = setup();
    expect(screen.getByText(/apply.*filter|filter.*apply/i)).toBeVisible();
    expect(screen.getByText(/fan/i)).toBeVisible();
    expect(screen.getByText(/farthest normal/i)).toBeVisible();
    expect(screen.getByText(/retest|test again/i)).toBeVisible();
    expect(props.controller.start).not.toHaveBeenCalled();
    expect(props.micTest.startCalibration).not.toHaveBeenCalled();
    await user.click(screen.getByRole('button', { name: 'Start calibration' }));
    expect(props.controller.start).toHaveBeenCalledTimes(1);
  });

  it('shows why unapplied capture settings block Start', async () => {
    const controller = fakeController({
      canStart: false,
      message: 'Apply microphone processing and level changes before calibration.',
    });
    const { user } = setup({ controller });
    const start = screen.getByRole('button', { name: 'Start calibration' });
    expect(start).toBeDisabled();
    expect(screen.getByText(controller.state.message ?? '')).toBeVisible();
    await user.click(start);
    expect(controller.start).not.toHaveBeenCalled();
  });

  it('allows Cancel while microphone permission is still pending', async () => {
    const controller = fakeController({ phase: 'acquiring' });
    const { user } = setup({ controller, micTest: fakeMicTest({ isPending: true }) });
    const cancel = screen.getByRole('button', { name: 'Cancel calibration' });
    expect(cancel).toBeEnabled();
    await user.click(cancel);
    expect(controller.cancel).toHaveBeenCalledTimes(1);
  });

  it('offers the labeled flow explicitly from Manual mode', async () => {
    const { props, user } = setup({ mode: 'manualCalibrate' });
    expect(screen.getByRole('slider', { name: 'Gate Threshold' })).toBeEnabled();
    expect(props.controller.start).not.toHaveBeenCalled();
    await user.click(screen.getByRole('button', { name: 'Suggest a starting point' }));
    expect(props.controller.start).toHaveBeenCalledTimes(1);
  });

  it.each(['autoCalibrate', 'manualCalibrate'] as const)(
    'keeps a native integer threshold control in %s',
    async (mode) => {
      const { props } = setup({ mode });
      const slider = screen.getByRole('slider', { name: 'Gate Threshold' });
      expect(slider).toHaveAttribute('type', 'range');
      expect(slider).toHaveAttribute('min', '-80');
      expect(slider).toHaveAttribute('max', '-20');
      expect(slider).toHaveAttribute('step', '1');
      expect(slider).toHaveValue('-50');
      fireEvent.change(slider, { target: { value: '-44' } });
      expect(props.onThresholdChange).toHaveBeenCalledWith(-44);
    }
  );

  it('keeps Start keyboard reachable without stealing focus on countdown updates', async () => {
    const { props, user, update } = setup();
    const start = screen.getByRole('button', { name: 'Start calibration' });
    start.focus();
    await user.keyboard('{Enter}');
    expect(props.controller.start).toHaveBeenCalledTimes(1);
    update({ controller: fakeController({ phase: 'quiet', secondsRemaining: 9 }) });
    const cancel = screen.getByRole('button', { name: 'Cancel calibration' });
    cancel.focus();
    update({ controller: fakeController({ phase: 'quiet', secondsRemaining: 8 }) });
    expect(screen.getByRole('button', { name: 'Cancel calibration' })).toHaveFocus();
  });
});

describe('NoiseGateCalibration labeled capture phases', () => {
  it.each(['quiet', 'readyToRead', 'speech'] as const)(
    'identifies active capture and permits Cancel during %s',
    async (phase) => {
      const controller = fakeController({ phase });
      const { user } = setup({ controller, micTest: fakeMicTest({ isTesting: true }) });
      expect(screen.getByText('Microphone active')).toBeVisible();
      const cancel = screen.getByRole('button', { name: 'Cancel calibration' });
      expect(cancel).toBeEnabled();
      await user.click(cancel);
      expect(controller.cancel).toHaveBeenCalledTimes(1);
    }
  );

  it('renders the controller captured countdown without advancing it on a rerender', () => {
    const controller = fakeController({ phase: 'quiet', secondsRemaining: 7 });
    const { update } = setup({ controller });
    expect(screen.getByText(/7\s*(?:seconds|s)\b/i)).toBeVisible();
    update({ controller: { ...controller, state: { ...controller.state, meterDbfs: -61 } } });
    expect(screen.getByText(/7\s*(?:seconds|s)\b/i)).toBeVisible();
    update({ controller: { ...controller, state: { ...controller.state, secondsRemaining: 6 } } });
    expect(screen.getByText(/6\s*(?:seconds|s)\b/i)).toBeVisible();
  });

  it('waits for Start reading and presents the full reading sentence', async () => {
    const controller = fakeController({ phase: 'readyToRead' });
    const { user } = setup({ controller });
    expect(
      screen.getByText(
        'Soft winds whisper through the trees. Peter packs five bright blue boxes. How clearly can you hear my quiet words?'
      )
    ).toBeVisible();
    expect(controller.startReading).not.toHaveBeenCalled();
    await user.click(screen.getByRole('button', { name: 'Start reading' }));
    expect(controller.startReading).toHaveBeenCalledTimes(1);
  });

  it('enables Done reading only after the controller has enough captured evidence', async () => {
    const controller = fakeController({ phase: 'speech', canFinishReading: false });
    const { user, update } = setup({ controller });
    expect(screen.getByRole('button', { name: 'Done reading' })).toBeDisabled();
    await user.click(screen.getByRole('button', { name: 'Done reading' }));
    expect(controller.finishReading).not.toHaveBeenCalled();
    update({
      controller: { ...controller, state: { ...controller.state, canFinishReading: true } },
    });
    await user.click(screen.getByRole('button', { name: 'Done reading' }));
    expect(controller.finishReading).toHaveBeenCalledTimes(1);
  });

  it('announces phase changes politely without announcing each countdown or meter update', () => {
    const controller = fakeController({ phase: 'quiet', secondsRemaining: 9, meterDbfs: -62 });
    const { update } = setup({ controller });
    const quietAnnouncement = screen.getByRole('status').textContent;
    expect(quietAnnouncement).toMatch(/quiet|room|background/i);
    expect(screen.getByRole('status')).toHaveAttribute('aria-live', 'polite');
    update({
      controller: {
        ...controller,
        state: { ...controller.state, secondsRemaining: 8, meterDbfs: -48 },
      },
    });
    expect(screen.getByRole('status').textContent).toBe(quietAnnouncement);
    update({ controller: { ...controller, state: { ...controller.state, phase: 'readyToRead' } } });
    expect(screen.getByRole('status')).toHaveTextContent(/read/i);
    expect(screen.getByRole('status').textContent).not.toBe(quietAnnouncement);
  });
});

describe('NoiseGateCalibration meter and review', () => {
  it('exposes digital level and current threshold as text without clamping the numeric evidence', () => {
    setup({ controller: fakeController({ phase: 'quiet', meterDbfs: -112 }), threshold: -52 });
    const meter = screen.getByRole('meter', { name: 'Microphone level' });
    expect(meter).toBeVisible();
    expect(meter.tagName).toBe('METER');
    expect(meter).toHaveAttribute('value', '-112');
    expect(meter).toHaveAttribute('min', '-112');
    expect(meter).toHaveAttribute('max', '0');
    expect(screen.getByText(/[-−]112\s*dBFS/i)).toBeVisible();
    expect(screen.getByText(/current threshold.*[-−]52\s*dBFS/i)).toBeVisible();
  });

  it('distinguishes pending and applied thresholds and explains global Apply', () => {
    setup({ threshold: -44, appliedThreshold: -50 });
    expect(screen.getByText(/pending.*[-−]44\s*dBFS/i)).toBeVisible();
    expect(screen.getByText(/applied.*[-−]50\s*dBFS/i)).toBeVisible();
    expect(
      screen.getByText(/Apply.*(?:hear|audible|take effect)|(?:hear|audible|take effect).*Apply/i)
    ).toBeVisible();
  });

  it('shows stopped capture, a separate suggestion and explained review statistics', () => {
    setup({ controller: fakeController({ phase: 'review', result: recommendation }) });
    expect(screen.getByText(/capture stopped/i)).toBeVisible();
    expect(screen.queryByText('Microphone active')).not.toBeInTheDocument();
    expect(screen.getByText(/suggested threshold.*[-−]57\s*dBFS/i)).toBeVisible();
    expect(screen.getByText(/(?:ambient|room).*P95/i)).toBeVisible();
    expect(screen.getByText(/(?:voice|speech).*P10/i)).toBeVisible();
    expect(screen.getByText(/(?:voice|speech).*P90/i)).toBeVisible();
    expect(screen.getByText(/[-−]63\s*dBFS/i)).toBeVisible();
    expect(screen.getByText(/[-−]38\s*dBFS/i)).toBeVisible();
    expect(screen.getByText(/[-−]24\s*dBFS/i)).toBeVisible();
    expect(screen.getByText(/starting point/i)).toBeVisible();
    expect(screen.getByText(/quieter word|louder background/i)).toBeVisible();
    expect(screen.getByRole('status')).toHaveTextContent(/complete|ready|review|starting point/i);
  });

  it('uses a proposal only through the controller and leaves the current slider unchanged until updated', async () => {
    const controller = fakeController({ phase: 'review', result: recommendation });
    const { props, user } = setup({ controller });
    await user.click(screen.getByRole('button', { name: 'Use threshold' }));
    expect(controller.useThreshold).toHaveBeenCalledTimes(1);
    expect(props.onThresholdChange).not.toHaveBeenCalled();
    expect(screen.getByRole('slider', { name: 'Gate Threshold' })).toHaveValue('-50');
  });

  it('offers Retry and Cancel after a successful review', async () => {
    const controller = fakeController({ phase: 'review', result: recommendation });
    const { user } = setup({ controller });
    await user.click(screen.getByRole('button', { name: 'Retry' }));
    expect(controller.start).toHaveBeenCalledTimes(1);
    await user.click(screen.getByRole('button', { name: 'Cancel calibration' }));
    expect(controller.cancel).toHaveBeenCalledTimes(1);
  });

  it('explains overlap, preserves the current threshold and offers a fresh Retry', async () => {
    const controller = fakeController({ phase: 'refused', result: overlap });
    const { props, user } = setup({ controller });
    expect(screen.getByText(/Room and voice levels overlap/i)).toBeVisible();
    expect(screen.getByText(/Move the microphone closer/i)).toBeVisible();
    expect(screen.getByText(/capture stopped/i)).toBeVisible();
    expect(screen.queryByRole('button', { name: 'Use threshold' })).not.toBeInTheDocument();
    expect(screen.getByRole('slider', { name: 'Gate Threshold' })).toHaveValue('-50');
    expect(props.onThresholdChange).not.toHaveBeenCalled();
    await user.click(screen.getByRole('button', { name: 'Retry' }));
    expect(controller.start).toHaveBeenCalledTimes(1);
    expect(screen.getByRole('status')).toHaveTextContent(/overlap|no reliable|starting point/i);
  });

  it('presents capture errors as polite text while leaving cancellation available', async () => {
    const controller = fakeController({ phase: 'refused', message: 'Microphone access denied' });
    const { user } = setup({ controller });
    expect(screen.getByText('Microphone access denied')).toBeVisible();
    expect(screen.getByRole('status')).toHaveTextContent('Microphone access denied');
    await user.click(screen.getByRole('button', { name: 'Cancel calibration' }));
    expect(controller.cancel).toHaveBeenCalledTimes(1);
  });
});
