import { describe, it, expect, beforeEach, beforeAll, afterAll, afterEach } from 'vitest';
import { render, screen, fireEvent, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { server } from '../../../mocks/server';
import { resetAllStores } from '../../../helpers/store-helpers';
import { useAuthStore } from '@/renderer/stores/authStore';
import { clearInvitePreviewCache } from '@/renderer/hooks/useInvitePreview';
import { InviteEmbed } from '@/renderer/components/Chat/InviteEmbed';

const API_BASE = 'http://localhost:8080';
const validPreview = {
  server_name: 'Acme HQ',
  server_icon: '/api/v1/media/server-icons/abc',
  server_banner: null,
  member_count: 7,
  valid: true,
};

beforeAll(() => server.listen({ onUnhandledRequest: 'bypass' }));
afterAll(() => server.close());
afterEach(() => server.resetHandlers());

describe('InviteEmbed', () => {
  beforeEach(() => {
    resetAllStores();
    clearInvitePreviewCache();
    useAuthStore.getState().setAccessToken('mock-token');
  });

  it('shows a neutral skeleton while loading (never sender-controlled text)', () => {
    server.use(
      http.get(`${API_BASE}/api/v1/invites/:code`, async () => {
        await new Promise((r) => setTimeout(r, 50));
        return HttpResponse.json(validPreview);
      })
    );
    render(<InviteEmbed code="GHJKMNPQ" />);
    expect(screen.getByText(/loading invite/i)).toBeInTheDocument();
  });

  it('renders the authoritative server name + Join once resolved', async () => {
    server.use(http.get(`${API_BASE}/api/v1/invites/:code`, () => HttpResponse.json(validPreview)));
    render(<InviteEmbed code="GHJKMNPQ" />);
    await waitFor(() => expect(screen.getByText('Acme HQ')).toBeInTheDocument());
    expect(screen.getByText(/7 members/i)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: /join/i })).toBeInTheDocument();
  });

  it('shows an invalid state for valid:false', async () => {
    server.use(
      http.get(`${API_BASE}/api/v1/invites/:code`, () =>
        HttpResponse.json({ ...validPreview, valid: false })
      )
    );
    render(<InviteEmbed code="KKKKMNPQ" />);
    await waitFor(() => expect(screen.getByText(/invalid or has expired/i)).toBeInTheDocument());
  });

  it('joins on click and shows Joined', async () => {
    server.use(
      http.get(`${API_BASE}/api/v1/invites/:code`, () => HttpResponse.json(validPreview)),
      http.post(`${API_BASE}/api/v1/invites/join`, () =>
        HttpResponse.json({ server: { id: 'server-1', name: 'Acme HQ' }, role: 'member' })
      )
    );
    render(<InviteEmbed code="GHJKMNPQ" />);
    const joinBtn = await screen.findByRole('button', { name: /join/i });
    fireEvent.click(joinBtn);
    await waitFor(() => expect(screen.getByText(/joined/i)).toBeInTheDocument());
  });

  it('shows an error when the join fails', async () => {
    server.use(
      http.get(`${API_BASE}/api/v1/invites/:code`, () => HttpResponse.json(validPreview)),
      http.post(`${API_BASE}/api/v1/invites/join`, () =>
        HttpResponse.json({ error: 'expired' }, { status: 410 })
      )
    );
    render(<InviteEmbed code="GHJKMNPQ" />);
    const joinBtn = await screen.findByRole('button', { name: /join/i });
    fireEvent.click(joinBtn);
    await waitFor(() => expect(screen.getByText(/could not join/i)).toBeInTheDocument());
  });

  it('disables the Join button while joining', async () => {
    server.use(
      http.get(`${API_BASE}/api/v1/invites/:code`, () => HttpResponse.json(validPreview)),
      http.post(`${API_BASE}/api/v1/invites/join`, async () => {
        await new Promise((r) => setTimeout(r, 50));
        return HttpResponse.json({ server: { id: 'server-1', name: 'Acme HQ' }, role: 'member' });
      })
    );
    render(<InviteEmbed code="GHJKMNPQ" />);
    const joinBtn = await screen.findByRole('button', { name: /join/i });
    fireEvent.click(joinBtn);
    await waitFor(() => expect(screen.getByRole('button', { name: /joining/i })).toBeDisabled());
    await waitFor(() => expect(screen.getByText(/joined/i)).toBeInTheDocument());
  });
});
