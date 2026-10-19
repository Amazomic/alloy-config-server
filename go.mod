module alloy-config-server

go 1.22

// Версии зафиксированы намеренно: свежие connect-go требуют Go 1.26+,
// а эти собираются на Go 1.22–1.24 (проверено компиляцией).
require (
	connectrpc.com/connect v1.14.0
	github.com/grafana/alloy-remote-config v0.0.12
	golang.org/x/net v0.23.0
	golang.org/x/text v0.14.0
	google.golang.org/protobuf v1.32.0
	gopkg.in/yaml.v3 v3.0.1
)
