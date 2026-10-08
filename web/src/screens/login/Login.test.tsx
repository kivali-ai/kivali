import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { createMemoryRouter } from 'react-router';
import { RouterProvider } from 'react-router/dom';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { stubFetch } from '../../app/test-utils';
import { goLogin } from '../setup/fixtures';
import { Login, safeNext } from './Login';

function renderLogin(props: Parameters<typeof Login>[0], url = '/login') {
  const router = createMemoryRouter([{ path: '/login', element: <Login {...props} /> }], { initialEntries: [url] });
  render(<RouterProvider router={router} />);
}

afterEach(() => vi.unstubAllGlobals());

describe('Login', () => {
  it('reads the public login route and offers one Google button', async () => {
    const calls = stubFetch({ 'GET /api/v1/login': goLogin });
    const onSignIn = vi.fn();
    renderLogin({ onSignIn }, '/login?next=%2Fteam');
    expect(await screen.findByRole('heading', { level: 1, name: 'Plainsong' })).toBeInTheDocument();
    expect(screen.getByRole('img', { name: 'Plainsong' })).toBeInTheDocument();
    expect(screen.getByText('Sign in to continue.')).toBeInTheDocument();
    expect(screen.getByText('Only Plainsong’s owner can sign in.')).toBeInTheDocument();
    expect(screen.queryByText('Sign-in isn’t set up yet')).toBeNull();
    const button = screen.getByRole('button', { name: 'Sign in with Google' });
    expect(button).toBeEnabled();
    await userEvent.click(button);
    expect(onSignIn).toHaveBeenCalledWith('/team');
    expect(calls[0]?.url).toBe('/api/v1/login');
  });

  it('signs back in to the app by default', async () => {
    stubFetch({ 'GET /api/v1/login': goLogin });
    const onSignIn = vi.fn();
    renderLogin({ onSignIn });
    await userEvent.click(await screen.findByRole('button', { name: 'Sign in with Google' }));
    expect(onSignIn).toHaveBeenCalledWith('/');
  });

  it('says sign-in is not set up and who can fix it, with the button disabled', async () => {
    stubFetch({ 'GET /api/v1/login': { ...goLogin, auth_ready: false } });
    renderLogin({});
    expect(await screen.findByText('Sign-in isn’t set up yet')).toBeInTheDocument();
    expect(screen.getByText(/Whoever runs this Kivali server needs to connect Google sign-in/)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Sign in with Google' })).toBeDisabled();
    expect(screen.queryByText('Sign in to continue.')).toBeNull();
  });

  it('in dev mode says sign-in is bypassed and continues into the app', async () => {
    stubFetch({ 'GET /api/v1/login': { ...goLogin, auth_ready: false, dev_mode: true } });
    const onContinue = vi.fn();
    renderLogin({ onContinue });
    expect(await screen.findByText('Sign-in is bypassed on this machine.')).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Sign in with Google' })).toBeNull();
    expect(screen.queryByText('Sign-in isn’t set up yet')).toBeNull();
    await userEvent.click(screen.getByRole('button', { name: 'Continue' }));
    expect(onContinue).toHaveBeenCalledWith('/');
  });

  it('carries the requested page through Continue in dev mode', async () => {
    stubFetch({ 'GET /api/v1/login': { ...goLogin, dev_mode: true } });
    const onContinue = vi.fn();
    renderLogin({ onContinue }, '/login?next=%2Fwork');
    await userEvent.click(await screen.findByRole('button', { name: 'Continue' }));
    expect(onContinue).toHaveBeenCalledWith('/work');
  });

  it('sends an off-site next back to the root instead', async () => {
    stubFetch({ 'GET /api/v1/login': goLogin });
    const onSignIn = vi.fn();
    renderLogin({ onSignIn }, '/login?next=%2F%2Fevil.example%2F');
    await userEvent.click(await screen.findByRole('button', { name: 'Sign in with Google' }));
    expect(onSignIn).toHaveBeenCalledWith('/');
  });

  it('shows no org mark before the org has a name', async () => {
    stubFetch({ 'GET /api/v1/login': { ...goLogin, org: { name: '', has_logo: false } } });
    renderLogin({});
    expect(await screen.findByRole('heading', { level: 1, name: 'Sign in' })).toBeInTheDocument();
    expect(screen.getByText('Only the team’s owner can sign in.')).toBeInTheDocument();
  });

  it('carries the Kivali lockup at the foot', () => {
    stubFetch({ 'GET /api/v1/login': goLogin });
    renderLogin({});
    expect(screen.getByRole('img', { name: 'Kivali' })).toHaveAttribute('src', expect.stringContaining('logos/kivali-lockup.svg'));
  });
});

describe('safeNext', () => {
  it('keeps same-site paths and refuses anything else', () => {
    expect(safeNext('/team?x=1')).toBe('/team?x=1');
    expect(safeNext(null)).toBe('/');
    expect(safeNext('https://evil.example')).toBe('/');
    expect(safeNext('//evil.example')).toBe('/');
    expect(safeNext('/\\evil.example')).toBe('/');
  });
});
