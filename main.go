// Command plugin keeps a Silo library collection for every box set.
package main

import (
	_ "embed"
	"log/slog"
	"os"

	"github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/runtime"

	"github.com/neilmc78/silo-plugin-boxset-collections/internal/boxsets"
)

// version is set at build time via -ldflags "-X main.version=...".
var version string

//go:embed manifest.json
var manifestJSON []byte

func main() {
	// go-plugin forwards the plugin's stderr to the host log.
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	syncer := boxsets.New(logger, host{}, nil)
	syncer.Version = version
	runtime.ServeManifestWithOptions(manifestJSON, version,
		runtime.CapabilityServers{ScheduledTask: syncer, EventConsumer: syncer},
		runtime.WithConfigure(syncer.Configure),
	)
}
