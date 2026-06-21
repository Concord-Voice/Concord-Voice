import { vi, beforeAll, afterAll, beforeEach } from 'vitest';

let processExitSpy: ReturnType<typeof vi.spyOn> | undefined;

// Prevent process.exit() from killing the test runner
beforeAll(() => {
  processExitSpy = vi.spyOn(process, 'exit').mockImplementation(() => undefined as never);
});

afterAll(() => {
  processExitSpy?.mockRestore();
});

// Reset mocks between tests
beforeEach(() => {
  vi.clearAllMocks();
});
