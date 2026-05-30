import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, fireEvent, waitFor } from '@testing-library/react';
import GoLivePage from './GoLivePage';

const GRAPH = {
  nodes: [
    { id: 'k3s-p0a', title: 'Critical security bug', priority: 0, status: 'open', type: 'bug' },
    { id: 'k3s-p1a', title: 'High priority epic', priority: 1, status: 'open', type: 'epic' },
    { id: 'k3s-p3a', title: 'Low priority polish', priority: 3, status: 'open', type: 'task' },
  ],
  edges: [{ from: 'k3s-p1a', to: 'k3s-p0a', type: 'blocks' }],
};

beforeEach(() => {
  global.fetch = vi.fn().mockResolvedValue({
    ok: true,
    status: 200,
    json: async () => GRAPH,
  });
});

describe('GoLivePage', () => {
  it('fetches the graph and renders P-filter chips with per-priority counts', async () => {
    render(<GoLivePage onExit={() => {}} />);
    await waitFor(() => expect(screen.getByText(/3 open/)).toBeTruthy());
    // chip labels include counts: P0 (1), P1 (1), P3 (1)
    expect(screen.getByTitle(/critical/)).toHaveTextContent('P0 (1)');
    expect(screen.getByTitle(/high/)).toHaveTextContent('P1 (1)');
    expect(screen.getByTitle(/low —/)).toHaveTextContent('P3 (1)');
  });

  it('renders P0/P1/P2 nodes by default but hides P3', async () => {
    render(<GoLivePage onExit={() => {}} />);
    await waitFor(() => expect(screen.getByText('k3s-p0a')).toBeTruthy());
    expect(screen.getByText('k3s-p1a')).toBeTruthy();
    // P3 is off by default
    expect(screen.queryByText('k3s-p3a')).toBeNull();
  });

  it('toggling the P3 chip reveals the P3 node', async () => {
    render(<GoLivePage onExit={() => {}} />);
    await waitFor(() => expect(screen.getByText('k3s-p0a')).toBeTruthy());
    fireEvent.click(screen.getByTitle(/low —/));
    await waitFor(() => expect(screen.getByText('k3s-p3a')).toBeTruthy());
  });

  it('selecting a node shows its blocked-by / blocks relationships', async () => {
    render(<GoLivePage onExit={() => {}} />);
    await waitFor(() => expect(screen.getByText('k3s-p0a')).toBeTruthy());
    fireEvent.click(screen.getByText('k3s-p0a'));
    // p0a is blocked by p1a (edge from p1a -> p0a)
    await waitFor(() => expect(screen.getByText(/Blocked by \/ parent \(1\)/)).toBeTruthy());
    // The title now appears in both the list row AND the detail aside — both
    // are legitimate. Assert at least one rather than exactly one.
    expect(screen.getAllByText('Critical security bug').length).toBeGreaterThanOrEqual(1);
  });

  it('shows an error when the fetch fails', async () => {
    global.fetch = vi.fn().mockResolvedValue({ ok: false, status: 500 });
    render(<GoLivePage onExit={() => {}} />);
    await waitFor(() => expect(screen.getByText(/Failed to load beads graph/)).toBeTruthy());
  });

  it('defaults to list view and flags ready vs blocked issues', async () => {
    render(<GoLivePage onExit={() => {}} />);
    await waitFor(() => expect(screen.getByText('k3s-p0a')).toBeTruthy());
    // p0a is blocked by p1a → shows a blocker badge, not "ready".
    // p1a has nothing blocking it → "ready". Exactly one ready badge among
    // the default-visible P0/P1 set.
    expect(screen.getAllByText('ready').length).toBe(1);
    // The list shows full titles (the whole point of the list view).
    expect(screen.getByText('High priority epic')).toBeTruthy();
  });

  it('toggles between list and graph views', async () => {
    render(<GoLivePage onExit={() => {}} />);
    await waitFor(() => expect(screen.getByText('k3s-p0a')).toBeTruthy());
    // List view shows the ready/blocked badges; graph view does not.
    expect(screen.getAllByText('ready').length).toBe(1);
    fireEvent.click(screen.getByRole('button', { name: 'graph' }));
    await waitFor(() => expect(screen.queryByText('ready')).toBeNull());
    // node ids still render in the SVG
    expect(screen.getByText('k3s-p0a')).toBeTruthy();
    // flip back
    fireEvent.click(screen.getByRole('button', { name: 'list' }));
    await waitFor(() => expect(screen.getAllByText('ready').length).toBe(1));
  });
});
