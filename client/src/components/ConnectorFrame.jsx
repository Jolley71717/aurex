// ConnectorFrame embeds a connector's web UI in an iframe. The src is the
// connector's launch_url — an aurex-side proxy path (/connector/{id}/ or /gc)
// rather than the connector's raw origin, so it stays same-origin/same-scheme
// and the proxy can strip frame-blocking headers (see connector_proxy.go).
//
// Some apps use an asset scheme the proxy can't perfectly rewrite, so we
// always offer an "Open in new tab" escape hatch that targets the same proxy
// path — reachable from anywhere aurex is, including a phone on the tailnet.

const ICONS = {
  paperclip: '📎',
  gascity: '⛽',
  beads: '📿',
  hubspace: '💧',
  ollama: '🦙',
};

export function connectorIcon(type) {
  return ICONS[type] || '🔌';
}

export default function ConnectorFrame({ connector, onExit }) {
  const launchUrl = connector?.launch_url;
  const name = connector?.name || 'Integration';
  const icon = connectorIcon(connector?.type);

  return (
    <div className="flex h-full w-full flex-col bg-bg text-zinc-100">
      <header className="sticky top-0 z-10 flex items-center justify-between border-b border-line bg-bg/95 px-4 py-3 backdrop-blur">
        <h1 className="text-sm font-semibold text-zinc-200">
          {icon} {name}
        </h1>
        <div className="flex items-center gap-2">
          {launchUrl && (
            <a
              href={launchUrl}
              target="_blank"
              rel="noreferrer"
              className="rounded-md border border-line px-3 py-1.5 text-xs text-zinc-300 hover:border-aura/40 hover:text-aura"
            >
              Open in new tab ↗
            </a>
          )}
          <button
            onClick={onExit}
            className="rounded-md border border-line px-3 py-1.5 text-xs text-zinc-300 hover:border-aura/40 hover:text-aura"
          >
            ← Terminal
          </button>
        </div>
      </header>

      {launchUrl ? (
        <iframe
          src={launchUrl}
          title={name}
          className="min-h-0 w-full flex-1 border-0"
        />
      ) : (
        <div className="flex flex-1 items-center justify-center p-8 text-center text-sm text-zinc-500">
          {connector
            ? `${name} has no web UI configured. Set a web_url for this connector in Settings to launch it here.`
            : 'Connector not found. It may have been removed or disabled.'}
        </div>
      )}
    </div>
  );
}
