module entity-workbench-go/shellcmd

go 1.24

require (
	entity-workbench-go/entitysdk v0.0.0
	entity-workbench-go/inspect v0.0.0
	entity-workbench-go/workbench v0.0.0
	go.entitychurch.org/entity-core-go/core v0.8.0
	golang.org/x/text v0.21.0
)

require (
	entity-workbench-go/fetch v0.0.0
	github.com/fxamacker/cbor/v2 v2.9.0 // indirect
	github.com/mr-tron/base58 v1.2.0 // indirect
	github.com/x448/float16 v0.8.4 // indirect
)

replace (
	entity-workbench-go/entitysdk => ../entitysdk
	entity-workbench-go/inspect => ../inspect
	entity-workbench-go/workbench => ../workbench
	go.entitychurch.org/entity-core-go/core => ../../entity-core-go/core
)

replace entity-workbench-go/fetch => ../fetch
