import { describe, it, expect, vi } from 'vitest';
import { render, screen, fireEvent } from '@testing-library/react';
import Sidebar from './Sidebar';

// Minimal props so Sidebar renders without throwing; we only exercise the
// Integrations launcher section added for connector buttons.
const baseProps = {
  sessions: [],
  activeId: null,
  onSelect: () => {},
  onCreate: () => {},
  onRename: () => {},
  onDelete: () => {},
  pushSubscribed: false,
  pushIsSecure: true,
  connected: true,
  onOpenPush: () => {},
  open: true,
  onClose: () => {},
};

const CONNECTORS = [
  { id: 'paperclip-default', type: 'paperclip', name: 'Paperclip', launch_url: '/connector/paperclip-default/', health: { ok: true } },
  { id: 'gascity-default', type: 'gascity', name: 'Gas City', launch_url: '/gc', health: { ok: false, status: 'down' } },
];

describe('Sidebar integrations launcher', () => {
  it('renders a button per launchable connector', () => {
    render(<Sidebar {...baseProps} connectors={CONNECTORS} onOpenConnector={() => {}} />);
    expect(screen.getByText('Integrations')).toBeTruthy();
    expect(screen.getByText(/Paperclip/)).toBeTruthy();
    expect(screen.getByText(/Gas City/)).toBeTruthy();
  });

  it('calls onOpenConnector with the connector id when clicked', () => {
    const onOpenConnector = vi.fn();
    render(<Sidebar {...baseProps} connectors={CONNECTORS} onOpenConnector={onOpenConnector} />);
    fireEvent.click(screen.getByTitle('Open Paperclip'));
    expect(onOpenConnector).toHaveBeenCalledWith('paperclip-default');
  });

  it('omits the Integrations section when there are no connectors', () => {
    render(<Sidebar {...baseProps} connectors={[]} onOpenConnector={() => {}} />);
    expect(screen.queryByText('Integrations')).toBeNull();
  });
});
