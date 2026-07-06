# Runs the no-account public demo (same config as the Cursor/VS Code one-click
# installs) so directory build/introspection checks need no secrets — just a
# clean `initialize` + `tools/list` handshake over stdio.
FROM node:20-alpine

ENV LEMA_MCP_MODE=public
ENV LEMA_PUBLIC_REPO=react-rfcs

ENTRYPOINT ["npx", "-y", "lema-mcp@latest"]
