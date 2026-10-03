module github.com/lfsoft/sync

go 1.25

require (
	github.com/jackc/pgx/v5 v5.7.2
	github.com/lfsoft/shared v0.0.0
	go.uber.org/zap v1.27.0
	golang.org/x/sys v0.28.0
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/go-ole/go-ole v1.3.0 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/mattn/go-adodb v0.0.1 // indirect
	github.com/rogpeppe/go-internal v1.16.0 // indirect
	go.uber.org/multierr v1.10.0 // indirect
	golang.org/x/crypto v0.31.0 // indirect
	golang.org/x/net v0.24.0 // indirect
	golang.org/x/sync v0.10.0 // indirect
	golang.org/x/text v0.21.0 // indirect
)

replace github.com/lfsoft/shared => ../LFSoft-app/backend/shared
