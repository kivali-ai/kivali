import { render, screen } from '@testing-library/react';
import { createMemoryRouter } from 'react-router';
import { RouterProvider } from 'react-router/dom';
import { describe, expect, it, vi } from 'vitest';
import { ApiError } from '../api/client';
import { RouteError } from './RouteError';

function renderThrowing(error: unknown) {
  function Boom(): never {
    throw error;
  }
  const router = createMemoryRouter([{ path: '/', element: <Boom />, errorElement: <RouteError /> }]);
  vi.spyOn(console, 'error').mockImplementation(() => undefined);
  render(<RouterProvider router={router} />);
}

describe('RouteError', () => {
  it('shows an ApiError as what happened and who can fix it', () => {
    renderThrowing(new ApiError(500, 'The agent list could not be read.', 'Whoever runs this Kivali server can look into it.'));
    expect(screen.getByText('The agent list could not be read.')).toBeInTheDocument();
    expect(screen.getByText('Whoever runs this Kivali server can look into it.')).toBeInTheDocument();
  });

  it('never shows the message or stack of an unexpected error', () => {
    renderThrowing(new Error('undefined is not a function at render (Home.tsx:12)'));
    expect(screen.getByText('This page hit a problem.')).toBeInTheDocument();
    expect(screen.queryByText(/undefined is not a function/)).toBeNull();
    expect(screen.queryByText(/Home\.tsx/)).toBeNull();
    expect(screen.getByRole('button', { name: 'Reload' })).toBeInTheDocument();
  });
});
