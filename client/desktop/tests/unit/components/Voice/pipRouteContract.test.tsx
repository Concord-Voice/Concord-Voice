import React from 'react';
import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/react';
import { MemoryRouter, Routes, Route, useParams } from 'react-router';

/**
 * Route-contract regression for the react-router v7 -> v8 migration (#2438).
 *
 * `/pip/:pipId` is the app's only parameterised route, and PipWindow branches on
 * the `pipId` prefix ("controls-" / "frames-" / "screen-") to pick its render
 * mode. Its existing unit test mocks `useParams` outright, so the route pattern
 * itself has never been exercised against a real router.
 *
 * This file deliberately contains NO `vi.mock`. A mock keyed on a stale
 * specifier stops intercepting silently and leaves a green test that proves
 * nothing; a zero-mock test cannot rot that way. It also renders a local probe
 * rather than PipWindow, so it tests the routing contract without dragging in
 * PipVoiceClient, ParticipantTile, and the lucide-react icon set.
 *
 * `render` is imported from @testing-library/react directly, NOT from
 * ../../../test-utils — that helper wraps children in a BrowserRouter, which
 * would nest a second router inside this file's MemoryRouter.
 */

function PipIdProbe() {
  const { pipId } = useParams<{ pipId: string }>();
  return <span data-testid="pip-id">{pipId ?? '<undefined>'}</span>;
}

function renderAt(path: string) {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <Routes>
        <Route path="/pip/:pipId" element={<PipIdProbe />} />
        <Route path="*" element={<span data-testid="no-match" />} />
      </Routes>
    </MemoryRouter>
  );
}

describe('/pip/:pipId route contract', () => {
  it('resolves pipId for a screen-share PiP', () => {
    renderAt('/pip/screen-abc123');
    expect(screen.getByTestId('pip-id')).toHaveTextContent('screen-abc123');
  });

  it('resolves pipId for a controls PiP', () => {
    renderAt('/pip/controls-main');
    expect(screen.getByTestId('pip-id')).toHaveTextContent('controls-main');
  });

  it('does not match a bare /pip with no parameter', () => {
    renderAt('/pip');
    expect(screen.getByTestId('no-match')).toBeInTheDocument();
    expect(screen.queryByTestId('pip-id')).not.toBeInTheDocument();
  });
});
