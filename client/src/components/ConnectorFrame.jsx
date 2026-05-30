// ConnectorFrame embeds a connector's web UI in an iframe pointed at the
// connector's real web_url — its own HTTPS origin at root (e.g. a tailscale
// serve endpoint like https://<host>.ts.net:8443/), NOT a path-prefixed proxy.
//
// Why the real root URL and not an aurex proxy: connectors like Paperclip
// route on location.pathname, so hosting them under /connector/{id} breaks
// their router ("no company matches prefix CONNECTOR"). Embedding the origin
// at root sidesteps that. It only works when (a) the connector is HTTPS (so an
// HTTPS aurex page can embed it without a mixed-content block) and (b) it
// doesn't send X-Frame-Options/CSP frame-ancestors. The "Open in new tab"
// link is the escape hatch if a connector refuses to be framed.

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
  const launchUrl = connector?.web_url;
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
