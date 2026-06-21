import React from 'react';
import { render, RenderOptions } from '@testing-library/react';
import { BrowserRouter } from 'react-router-dom';
import userEventLib from '@testing-library/user-event';
import { ModalProvider } from '@/renderer/components/ui/ModalContext';

function AllProviders({ children }: { children: React.ReactNode }) {
  return (
    <BrowserRouter>
      <ModalProvider>{children}</ModalProvider>
    </BrowserRouter>
  );
}

function customRender(ui: React.ReactElement, options?: Omit<RenderOptions, 'wrapper'>) {
  return render(ui, { wrapper: AllProviders, ...options });
}

// Re-export everything from RTL
export * from '@testing-library/react';
export { userEventLib as userEvent };

// Override render with the custom wrapped version
export { customRender as render };
