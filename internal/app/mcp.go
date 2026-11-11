package app

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/rknightion/finstore/internal/config"
	"github.com/rknightion/finstore/internal/mcpserver"
	"github.com/rknightion/finstore/internal/semconv"
	"github.com/rknightion/finstore/internal/store"
	"github.com/rknightion/finstore/internal/telemetry"
)

// openMCP builds the /mcp handler, or returns nil when the endpoint is off. It is on only when
// FINSTORE_MCP_DATABASE_URL and FINSTORE_MCP_APP_CAP are both set; otherwise it logs which one is
// missing and /mcp answers 404.
//
// When it is on, it opens the reader as the finstore_mcp login with telemetry and ForAgentSQL (so no
// database span carries an agent's SQL or is named after it) and refuses to start unless the
// connection really is that role: a URL naming the app role would silently give agents write
// access. The handler it returns is behind mcpserver.Gate, which admits only callers whose tailnet
// grant carries the capability. The returned close function closes the reader.
func openMCP(ctx context.Context, cfg config.Config, providers *telemetry.Providers, metrics *telemetry.Metrics,
	log *slog.Logger, version string) (http.Handler, func(), error) {
	off := func(variable string) (http.Handler, func(), error) {
		log.InfoContext(ctx, "mcp endpoint off: "+variable+" unset")
		return nil, func() {}, nil
	}
	if cfg.MCPDatabaseURL == "" {
		return off(config.EnvMCPDatabaseURL)
	}
	if cfg.MCPAppCap == "" {
		return off(config.EnvMCPAppCap)
	}
	rd, err := store.OpenReader(ctx, cfg.MCPDatabaseURL,
		store.WithTelemetry(providers.TracerProvider, providers.MeterProvider), store.ForAgentSQL())
	if err != nil {
		return nil, nil, fmt.Errorf("mcp: %w", err)
	}
	if err := rd.CheckRole(ctx); err != nil {
		rd.Close()
		return nil, nil, fmt.Errorf("mcp: %s must log in as %s: %w", config.EnvMCPDatabaseURL, store.RoleMCP, err)
	}
	srv := mcpserver.NewServer(mcpserver.Options{
		Reader: mcpserver.FromStore(rd), Version: version,
		Tracer: providers.Tracer(semconv.ServiceName), Metrics: metrics, Log: log,
	})
	return mcpserver.Gate(cfg.MCPAppCap, mcpserver.Handler(srv)), rd.Close, nil
}
