module entity-workbench-go/avalonia/bridge

go 1.24

require (
	entity-workbench-go/entitysdk v0.0.0
	entity-workbench-go/programs v0.0.0
	entity-workbench-go/shellboot v0.0.0
	entity-workbench-go/shellcmd v0.0.0
)

require (
	go.entitychurch.org/entity-core-go/core v0.8.0 // indirect
	go.entitychurch.org/entity-core-go/ext v0.8.0 // indirect
	entity-workbench-go/shellpanel v0.0.0 // indirect
	entity-workbench-go/workbench v0.0.0 // indirect
	github.com/fxamacker/cbor/v2 v2.9.0 // indirect
	github.com/mr-tron/base58 v1.2.0 // indirect
	github.com/x448/float16 v0.8.4 // indirect
)

replace (
	go.entitychurch.org/entity-core-go/core => ../../../entity-core-go/core
	go.entitychurch.org/entity-core-go/ext => ../../../entity-core-go/ext
	entity-workbench-go/entitysdk => ../../entitysdk
	entity-workbench-go/programs => ../../programs
	entity-workbench-go/shellboot => ../../shellboot
	entity-workbench-go/shellcmd => ../../shellcmd
	entity-workbench-go/shellpanel => ../../shellpanel
	entity-workbench-go/workbench => ../../workbench
)
