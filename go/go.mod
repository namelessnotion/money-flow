module github.com/namelessnotion/money_flow/go

go 1.27.0

tool (
	github.com/arthurnn/twirp-ruby/protoc-gen-twirp_ruby
	github.com/twitchtv/twirp/protoc-gen-twirp
	google.golang.org/protobuf/cmd/protoc-gen-go
)

require (
	github.com/jackc/pgx/v5 v5.10.0
	github.com/segmentio/kafka-go v0.4.51
	github.com/tigerbeetle/tigerbeetle-go v0.17.9
	github.com/twitchtv/twirp v8.1.3+incompatible
	google.golang.org/protobuf v1.36.12
)

require (
	github.com/arthurnn/twirp-ruby v1.14.1 // indirect
	github.com/davecgh/go-spew v1.1.2-0.20180830191138-d8f796af33cc // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/klauspost/compress v1.15.9 // indirect
	github.com/pierrec/lz4/v4 v4.1.15 // indirect
	github.com/pkg/errors v0.9.1 // indirect
	github.com/pmezard/go-difflib v1.0.1-0.20181226105442-5d4384ee4fb2 // indirect
	golang.org/x/sync v0.18.0 // indirect
	golang.org/x/text v0.31.0 // indirect
)
