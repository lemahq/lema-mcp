## CORS Middleware Optimization

- In `cmd/lema-mcp/serve.go`, optimized the CORS middleware to parse the `LEMA_HTTP_ORIGIN` environment variable only once at server startup rather than on every request.
- This provides a minor performance improvement by reducing allocations and string operations per request, while securely supporting a comma-separated list of allowed origins.
