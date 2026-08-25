module entity-workbench-go/entity-fetch

go 1.25.0

require entity-workbench-go/fetch v0.0.0

require (
	github.com/cloudflare/circl v1.6.3 // indirect
	golang.org/x/crypto v0.53.0 // indirect
	golang.org/x/sys v0.46.0 // indirect
)

require (
	entity-workbench-go/entitysdk v0.0.0 // indirect
	github.com/fxamacker/cbor/v2 v2.9.0 // indirect
	github.com/mr-tron/base58 v1.2.0 // indirect
	github.com/x448/float16 v0.8.4 // indirect
	go.entitychurch.org/entity-core-go/core v0.8.0
)

replace (
	entity-workbench-go/fetch => ../fetch
	go.entitychurch.org/entity-core-go/core => ../../entity-core-go/core
	go.entitychurch.org/entity-core-go/ext => ../../entity-core-go/ext
)

replace entity-workbench-go/entitysdk => ../entitysdk
