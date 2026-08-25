package workbench

// Re-exports from entitysdk package.
//
// These type aliases and function references allow console/ and canvas/
// to continue importing workbench as "wb" without changes. The actual
// SDK implementation lives in entitysdk/. When renderers are updated
// to import entitysdk directly, this file can be removed.
//
// UI-layer types (layout, tree, hex dump, entity header, output line
// flatteners) are defined directly in this package — they are
// workbench-specific and do not live in the SDK. See ui_*.go.

import "entity-workbench-go/entitysdk"

// --- Type aliases (SDK types) ---

type AppPeer = entitysdk.AppPeer
type Executor = entitysdk.Executor
type Store = entitysdk.Store
type DispatchFunc = entitysdk.DispatchFunc
type PeerContext = entitysdk.PeerContext
type WorkspaceState = entitysdk.WorkspaceState
type Selection = entitysdk.Selection
type ResolvedEntity = entitysdk.ResolvedEntity
type EventLog = entitysdk.EventLog
type LogLevel = entitysdk.LogLevel
type LogEntry = entitysdk.LogEntry
type FormattedValue = entitysdk.FormattedValue
type FormattedLine = entitysdk.FormattedLine
type ValueKind = entitysdk.ValueKind
type HandlerInfo = entitysdk.HandlerInfo
type Error = entitysdk.Error
type StoreWatch = entitysdk.StoreWatch
type ChangeEvent = entitysdk.ChangeEvent
type ChangeEventType = entitysdk.ChangeEventType
type PeerConfig = entitysdk.PeerConfig
type StorageConfig = entitysdk.StorageConfig
type HandlerRegistration = entitysdk.HandlerRegistration
type Grant = entitysdk.Grant
type GrantScope = entitysdk.GrantScope
type ScopeDimension = entitysdk.ScopeDimension
type Scope = entitysdk.Scope
type Connection = entitysdk.Connection
type PeerLiveness = entitysdk.PeerLiveness

// The §3.13 peer lifecycle enum, re-exported for renderers.
const (
	PeerStatusConnected    = entitysdk.PeerStatusConnected
	PeerStatusSuspect      = entitysdk.PeerStatusSuspect
	PeerStatusDisconnected = entitysdk.PeerStatusDisconnected
)

// --- Constants (SDK) ---

const (
	LogInfo    = entitysdk.LogInfo
	LogVerbose = entitysdk.LogVerbose
	LogDebug   = entitysdk.LogDebug

	KindNull    = entitysdk.KindNull
	KindBool    = entitysdk.KindBool
	KindString  = entitysdk.KindString
	KindNumber  = entitysdk.KindNumber
	KindBytes   = entitysdk.KindBytes
	KindHash    = entitysdk.KindHash
	KindKey     = entitysdk.KindKey
	KindIndex   = entitysdk.KindIndex
	KindUnknown = entitysdk.KindUnknown
	KindError   = entitysdk.KindError
	KindPath    = entitysdk.KindPath

	ChangePut    = entitysdk.ChangePut
	ChangeRemove = entitysdk.ChangeRemove

	DefaultAppID = entitysdk.DefaultAppID
)

// --- Function re-exports (SDK) ---

var (
	NewAppPeer = entitysdk.NewAppPeer
	CreatePeer = entitysdk.CreatePeer

	WildcardGrant      = entitysdk.WildcardGrant
	WildcardGrantScope = entitysdk.WildcardGrantScope

	NewExecutor          = entitysdk.NewExecutor
	NewStore             = entitysdk.NewStore
	NewPeerContext       = entitysdk.NewPeerContext
	NewWorkspaceState    = entitysdk.NewWorkspaceState
	NewWorkspaceStateFor = entitysdk.NewWorkspaceStateFor
	NewEventLog          = entitysdk.NewEventLog

	NewError          = entitysdk.NewError
	WrapError         = entitysdk.WrapError
	ErrorFromResponse = entitysdk.ErrorFromResponse
	StatusOf          = entitysdk.StatusOf
	IsStatus          = entitysdk.IsStatus
	IsNotFound        = entitysdk.IsNotFound
	IsForbidden       = entitysdk.IsForbidden
	IsConflict        = entitysdk.IsConflict
	IsRateLimited     = entitysdk.IsRateLimited
	IsNotSupported    = entitysdk.IsNotSupported
	IsClientError     = entitysdk.IsClientError
	IsAuthError       = entitysdk.IsAuthError
	IsSystemError     = entitysdk.IsSystemError

	ResolveEntity     = entitysdk.ResolveEntity
	DecodeEntityData  = entitysdk.DecodeEntityData
	ListByPrefix      = entitysdk.ListByPrefix
	ListEntriesSorted = entitysdk.ListEntriesSorted

	FormatCBOR      = entitysdk.FormatCBOR
	FormatValue     = entitysdk.FormatValue
	IsSimpleValue   = entitysdk.IsSimpleValue
	SortedMapKeys   = entitysdk.SortedMapKeys
	RenderPlainText = entitysdk.RenderPlainText

	DiscoverHandlers            = entitysdk.DiscoverHandlers
	DiscoverHandlersFromEntries = entitysdk.DiscoverHandlersFromEntries

	// Note: Query and QueryCount are methods on Executor, not
	// standalone functions. They're available via any Executor
	// instance. The kernel types (QueryExpressionData,
	// QueryResultData, etc.) live in entity-core-go/core/types and
	// are imported directly where needed.
)
