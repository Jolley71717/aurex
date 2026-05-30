import { describe, it, expect, vi } from 'vitest';
import { render, screen, fireEvent } from '@testing-library/react';
import ConnectorFrame, { connectorIcon } from './ConnectorFrame';

describe('connectorIcon', () => {
  it('maps known types and falls back for unknown', () => {
    expect(connectorIcon('paperclip')).toBe('📎');
    expect(connectorIcon('gascity')).toBe('⛽');
    expect(connectorIcon('mystery')).toBe('🔌');
    expect(connectorIcon(undefined)).toBe('🔌');
  });
});

describe('ConnectorFrame', () => {
  const paperclip = {
    id: 'paperclip-default',
    type: 'paperclip',
    name: 'Paperclip',
    launch_url: '/connector/paperclip-default/',
  };

  it('embeds the launch_url in an iframe and offers a new-tab link', () => {
    render(<ConnectorFrame connector={paperclip} onExit={() => {}} />);

    const iframe = document.querySelector('iframe');
    expect(iframe).toBeTruthy();
    expect(iframe.getAttribute('src')).toBe('/connector/paperclip-default/');
    expect(iframe.getAttribute('title')).toBe('Paperclip');

    const newTab = screen.getByText(/Open in new tab/i);
    expect(newTab.getAttribute('href')).toBe('/connector/paperclip-default/');
    expect(newTab.getAttribute('target')).toBe('_blank');
  });

  it('calls onExit when the Terminal button is clicked', () => {
    const onExit = vi.fn();
    render(<ConnectorFrame connector={paperclip} onExit={onExit} />);
    fireEvent.click(screen.getByText(/← Terminal/));
    expect(onExit).toHaveBeenCalledTimes(1);
  });

  it('shows a not-found message when the connector is missing', () => {
    render(<ConnectorFrame connector={null} onExit={() => {}} />);
    expect(document.querySelector('iframe')).toBeNull();
    expect(screen.getByText(/Connector not found/i)).toBeTruthy();
  });

  it('shows a no-web-UI message when launch_url is absent', () => {
    render(
      <ConnectorFrame
        connector={{ id: 'beads', type: 'beads', name: 'Beads' }}
        onExit={() => {}}
      />
    );
    expect(document.querySelector('iframe')).toBeNull();
    expect(screen.getByText(/no web UI configured/i)).toBeTruthy();
  });
});
