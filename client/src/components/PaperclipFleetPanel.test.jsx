import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, waitFor, fireEvent } from '@testing-library/react';
import PaperclipFleetPanel from './PaperclipFleetPanel';

// These tests drive PaperclipFleetPanel against mocked fetch responses to the
// two endpoints the panel hits: GET /api/paperclip-agents and POST
// /api/paperclip-agents/{id}/wake. They cover the rendered list, the Wake
// button POST behavior, error rendering, and the per-row "queued" flash.

const SAMPLE_AGENTS = [
  {
    id: 'ceo-uuid',
    name: 'CEO',
    role: 'ceo',
    title: 'Chief Exec',
    status: 'idle',
    heartbeat_sec: 14400,
    heartbeat_enabled: true,
  },
  {
    id: 'eng-uuid',
    name: 'Engineer',
    role: 'engineer',
    status: 'running',
    heartbeat_sec: 7200,
    heartbeat_enabled: true,
  },
  {
    id: 'jan-uuid',
    name: 'Janitor',
    role: 'devops',
    status: 'idle',
    heartbeat_enabled: false,
  },
];

beforeEach(() => {
  // Default success response for the list endpoint. Individual tests can
  // override fetch to test wake / error paths.
  global.fetch = vi.fn().mockResolvedValue({
    ok: true,
    status: 200,
    json: async () => ({ agents: SAMPLE_AGENTS }),
  });
});

afterEach(() => {
  vi.restoreAllMocks();
});

describe('PaperclipFleetPanel', () => {
  it('renders the three sample agents with status badges and heartbeat intervals', async () => {
    render(<PaperclipFleetPanel />);
    await waitFor(() => expect(screen.getByText('CEO')).toBeInTheDocument());

    expect(screen.getByText('CEO')).toBeInTheDocument();
    expect(screen.getByText('Engineer')).toBeInTheDocument();
    expect(screen.getByText('Janitor')).toBeInTheDocument();

    // Heartbeat interval formatting: 14400s -> "4 h", 7200s -> "2 h", off -> "off".
    expect(screen.getByText(/heartbeat 4 h/)).toBeInTheDocument();
    expect(screen.getByText(/heartbeat 2 h/)).toBeInTheDocument();
    expect(screen.getByText(/heartbeat off/)).toBeInTheDocument();

    // Status badges
    expect(screen.getAllByText('idle').length).toBeGreaterThanOrEqual(2);
    expect(screen.getByText('running')).toBeInTheDocument();
  });

  it('shows a loading state then resolves', async () => {
    let resolveFetch;
    global.fetch = vi.fn().mockReturnValue(
      new Promise((res) => {
        resolveFetch = () => res({ ok: true, status: 200, json: async () => ({ agents: [] }) });
      })
    );
    render(<PaperclipFleetPanel />);
    expect(screen.getByText(/Loading Paperclip fleet/i)).toBeInTheDocument();
    resolveFetch();
    await waitFor(() => expect(screen.queryByText(/Loading Paperclip fleet/i)).not.toBeInTheDocument());
  });

  it('renders an error banner when the list endpoint returns a non-2xx', async () => {
    global.fetch = vi.fn().mockResolvedValue({
      ok: false,
      status: 503,
      json: async () => ({ error: 'no Paperclip board token' }),
    });
    render(<PaperclipFleetPanel />);
    await waitFor(() => expect(screen.getByText(/Paperclip fleet unavailable/i)).toBeInTheDocument());
    expect(screen.getByText('no Paperclip board token')).toBeInTheDocument();
  });

  it('renders the same error banner when fetch itself rejects (network failure)', async () => {
    global.fetch = vi.fn().mockRejectedValue(new Error('connect ECONNREFUSED'));
    render(<PaperclipFleetPanel />);
    await waitFor(() => expect(screen.getByText(/Paperclip fleet unavailable/i)).toBeInTheDocument());
    expect(screen.getByText(/ECONNREFUSED/)).toBeInTheDocument();
  });

  it('Wake button POSTs to the correct per-agent endpoint with {} body and shows the queued flash', async () => {
    const fetchMock = vi.fn();
    // First call: list. Subsequent: wake.
    fetchMock.mockImplementation((url, opts) => {
      if (url === '/api/paperclip-agents' && (!opts || opts.method === undefined || opts.method === 'GET')) {
        return Promise.resolve({ ok: true, status: 200, json: async () => ({ agents: SAMPLE_AGENTS }) });
      }
      if (url === '/api/paperclip-agents/ceo-uuid/wake' && opts?.method === 'POST') {
        return Promise.resolve({ ok: true, status: 202, json: async () => ({ status: 'queued' }) });
      }
      return Promise.reject(new Error(`unexpected fetch: ${url}`));
    });
    global.fetch = fetchMock;

    render(<PaperclipFleetPanel />);
    await waitFor(() => expect(screen.getByText('CEO')).toBeInTheDocument());

    // Find the Wake button on the CEO row. There are three buttons; pick by index
    // since they all read "⚡ Wake" — the order matches SAMPLE_AGENTS.
    const wakeButtons = screen.getAllByRole('button', { name: /Wake/ });
    expect(wakeButtons).toHaveLength(3);
    fireEvent.click(wakeButtons[0]);

    // Assert the POST happened with the right shape.
    await waitFor(() => {
      const wakeCalls = fetchMock.mock.calls.filter(
        ([, opts]) => opts?.method === 'POST'
      );
      expect(wakeCalls).toHaveLength(1);
      expect(wakeCalls[0][0]).toBe('/api/paperclip-agents/ceo-uuid/wake');
      expect(wakeCalls[0][1].body).toBe('{}');
      expect(wakeCalls[0][1].headers['Content-Type']).toBe('application/json');
    });

    // The "queued" flash should render next to the CEO row.
    await waitFor(() => expect(screen.getByText(/queued/)).toBeInTheDocument());
  });

  it('Wake button shows the failure flash when the wake endpoint returns an error', async () => {
    const fetchMock = vi.fn();
    fetchMock.mockImplementation((url, opts) => {
      if (opts?.method === 'POST') {
        return Promise.resolve({
          ok: false,
          status: 403,
          json: async () => ({ error: 'agent not found' }),
        });
      }
      return Promise.resolve({ ok: true, status: 200, json: async () => ({ agents: SAMPLE_AGENTS }) });
    });
    global.fetch = fetchMock;

    render(<PaperclipFleetPanel />);
    await waitFor(() => expect(screen.getByText('CEO')).toBeInTheDocument());

    const wakeButtons = screen.getAllByRole('button', { name: /Wake/ });
    fireEvent.click(wakeButtons[1]);  // Engineer

    await waitFor(() => expect(screen.getByText(/failed: agent not found/)).toBeInTheDocument());
  });

  it('does not double-fire if the user double-taps Wake (button disabled during in-flight)', async () => {
    let resolveWake;
    const wakePromise = new Promise((res) => {
      resolveWake = () => res({ ok: true, status: 202, json: async () => ({ status: 'queued' }) });
    });

    const fetchMock = vi.fn().mockImplementation((url, opts) => {
      if (opts?.method === 'POST') return wakePromise;
      return Promise.resolve({ ok: true, status: 200, json: async () => ({ agents: SAMPLE_AGENTS }) });
    });
    global.fetch = fetchMock;

    render(<PaperclipFleetPanel />);
    await waitFor(() => expect(screen.getByText('CEO')).toBeInTheDocument());

    const wakeButtons = screen.getAllByRole('button', { name: /Wake/ });
    fireEvent.click(wakeButtons[0]);

    // Button should be disabled while the POST is in flight.
    await waitFor(() => {
      const btn = screen.getAllByRole('button').find((b) => b.textContent === '…');
      expect(btn).toBeDefined();
      expect(btn).toBeDisabled();
    });

    // A second click on the disabled button must not produce a second POST.
    const disabledBtn = screen.getAllByRole('button').find((b) => b.textContent === '…');
    fireEvent.click(disabledBtn);

    resolveWake();
    await waitFor(() => {
      const wakeCalls = fetchMock.mock.calls.filter(([, opts]) => opts?.method === 'POST');
      expect(wakeCalls).toHaveLength(1);
    });
  });
});
