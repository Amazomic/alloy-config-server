module alloy-configuration-hub

go 1.26.0

// Dependency versions are pinned; Go 1.26 is required by the current RPC stack.
require (
	connectrpc.com/connect v1.21.1
	github.com/grafana/alloy-remote-config v0.0.12
	golang.org/x/net v0.61.0
	golang.org/x/text v0.43.0 // indirect
	google.golang.org/protobuf v1.36.12
)
