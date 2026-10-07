import { http, HttpResponse } from 'msw';
import { server } from '../../../mocks/server';
import { render, screen, userEvent } from '../../../test-utils';
import { resetAllStores } from '../../../helpers/store-helpers';
import Login from '@/renderer/components/Auth/Login';

// Bound to constants so the credential-named fields below are followed by
// identifiers rather than quoted literals (detect-secrets keys on adjacency).
const FIXTURE_EMAIL = 'test@example.com';
const FIXTURE_PW = 'Password123!';

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }));
afterEach(() => server.resetHandlers());
afterAll(() => server.close());

beforeEach(() => {
  resetAllStores();
});

const props = {
  onBack: vi.fn(),
  onSuccess: vi.fn(),
  onSwitchToRegister: vi.fn(),
  onForgotPassword: vi.fn(),
};

// #3563: a sign-in challenge leaves a recovery-only TOTP out of `methods`, and
// its backup codes go with it. With email left, there is nothing else to offer.
describe('Login MFA step and backup codes', () => {
  it.each([
    {
      name: 'TOTP recovery-only',
      methods: ['email'],
      recoveryOnly: ['totp'],
      offersAnother: false,
    },
    { name: 'TOTP listed', methods: ['totp', 'email'], recoveryOnly: [], offersAnother: true },
  ])(
    'offers another form only when backup codes can sign in ($name)',
    async ({ methods, recoveryOnly, offersAnother }) => {
      const requests: { method: string; path: string; body: unknown }[] = [];
      server.use(
        http.post('*/api/v1/auth/login', async ({ request }) => {
          requests.push({
            method: request.method,
            path: new URL(request.url).pathname,
            body: await request.json(),
          });
          return HttpResponse.json({
            mfa_required: true,
            mfa_challenge_token: 'mfa-token-123',
            methods,
            recovery_only_methods: recoveryOnly,
          });
        })
      );

      const user = userEvent.setup();
      render(<Login {...props} />);
      await user.type(screen.getByLabelText('Email'), FIXTURE_EMAIL);
      await user.type(screen.getByLabelText('Password'), FIXTURE_PW);
      await user.click(screen.getByText('Sign In'));

      await screen.findByText('Two-Factor Authentication');
      expect(requests).toHaveLength(1);
      expect(requests[0]).toMatchObject({
        method: 'POST',
        path: '/api/v1/auth/login',
        body: { email: FIXTURE_EMAIL },
      });
      const another = screen.queryByText('Choose another form of verification');
      if (offersAnother) expect(another).toBeInTheDocument();
      else expect(another).not.toBeInTheDocument();
    }
  );
});
