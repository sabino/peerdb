package peerflow

import (
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/log"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
	"google.golang.org/protobuf/proto"

	"github.com/PeerDB-io/peerdb/flow/generated/protos"
	"github.com/PeerDB-io/peerdb/flow/internal"
	"github.com/PeerDB-io/peerdb/flow/model"
	"github.com/PeerDB-io/peerdb/flow/shared"
)

type CDCFlowWorkflowState struct {
	// flow config update request, set to nil after processed
	FlowConfigUpdate *protos.CDCFlowConfigUpdate
	// options passed to all SyncFlows
	SyncFlowOptions *protos.SyncFlowOptions
	// for becoming DropFlow
	DropFlowInput *protos.DropFlowInput
	// used for computing backoff timeout
	LastError  time.Time
	ErrorCount int32
	// Current signalled state of the peer flow.
	ActiveSignal      model.CDCFlowSignal
	CurrentFlowStatus protos.FlowStatus

	// Initial load settings
	SnapshotNumRowsPerPartition uint32
	SnapshotMaxParallelWorkers  uint32
	SnapshotNumTablesInParallel uint32
}

// returns a new empty PeerFlowState
func NewCDCFlowWorkflowState(ctx workflow.Context, logger log.Logger, cfg *protos.FlowConnectionConfigs) *CDCFlowWorkflowState {
	tableMappings := make([]*protos.TableMapping, 0, len(cfg.TableMappings))
	for _, tableMapping := range cfg.TableMappings {
		tableMappings = append(tableMappings, proto.CloneOf(tableMapping))
	}
	state := CDCFlowWorkflowState{
		ActiveSignal:      model.NoopSignal,
		CurrentFlowStatus: protos.FlowStatus_STATUS_SETUP,
		FlowConfigUpdate:  nil,
		SyncFlowOptions: &protos.SyncFlowOptions{
			BatchSize:          cfg.MaxBatchSize,
			IdleTimeoutSeconds: cfg.IdleTimeoutSeconds,
			TableMappings:      tableMappings,
			NumberOfSyncs:      0,
		},
		SnapshotNumRowsPerPartition: cfg.SnapshotNumRowsPerPartition,
		SnapshotMaxParallelWorkers:  cfg.SnapshotMaxParallelWorkers,
		SnapshotNumTablesInParallel: cfg.SnapshotNumTablesInParallel,
	}
	syncStatusToCatalog(ctx, workflow.GetLogger(ctx), state.CurrentFlowStatus)
	return &state
}

func syncStatusToCatalog(ctx workflow.Context, logger log.Logger, status protos.FlowStatus) {
	updateCtx := workflow.WithLocalActivityOptions(ctx, workflow.LocalActivityOptions{
		StartToCloseTimeout: 1 * time.Minute,
	})

	updateFuture := workflow.ExecuteLocalActivity(updateCtx, updateFlowStatusInCatalogActivity,
		workflow.GetInfo(ctx).WorkflowExecution.ID, status)
	if err := updateFuture.Get(updateCtx, nil); err != nil {
		logger.Warn("Failed to update flow status in catalog", slog.Any("error", err), slog.String("flowStatus", status.String()))
	}
}

func (s *CDCFlowWorkflowState) updateStatus(ctx workflow.Context, logger log.Logger, newStatus protos.FlowStatus) {
	s.CurrentFlowStatus = newStatus
	// update the status in the catalog
	syncStatusToCatalog(ctx, logger, s.CurrentFlowStatus)
}

func GetUUID(ctx workflow.Context) string {
	return GetSideEffect(ctx, func(_ workflow.Context) string {
		return uuid.New().String()
	})
}

func GetChildWorkflowID(
	prefix string,
	peerFlowName string,
	uuid string,
) string {
	return fmt.Sprintf("%s-%s-%s", prefix, peerFlowName, uuid)
}

func updateFlowConfigWithLatestSettings(
	cfg *protos.FlowConnectionConfigs,
	state *CDCFlowWorkflowState,
) *protos.FlowConnectionConfigs {
	cloneCfg := proto.CloneOf(cfg)
	cloneCfg.MaxBatchSize = state.SyncFlowOptions.BatchSize
	cloneCfg.IdleTimeoutSeconds = state.SyncFlowOptions.IdleTimeoutSeconds
	cloneCfg.TableMappings = state.SyncFlowOptions.TableMappings
	cloneCfg.SnapshotNumRowsPerPartition = state.SnapshotNumRowsPerPartition
	cloneCfg.SnapshotMaxParallelWorkers = state.SnapshotMaxParallelWorkers
	cloneCfg.SnapshotNumTablesInParallel = state.SnapshotNumTablesInParallel
	return cloneCfg
}

// CDCFlowWorkflowResult is the result of the PeerFlowWorkflow.
type CDCFlowWorkflowResult = CDCFlowWorkflowState

func syncStateToConfigProtoInCatalog(
	ctx workflow.Context,
	cfg *protos.FlowConnectionConfigs,
	state *CDCFlowWorkflowState,
) *protos.FlowConnectionConfigs {
	cloneCfg := updateFlowConfigWithLatestSettings(cfg, state)
	uploadConfigToCatalog(ctx, cloneCfg)
	return cloneCfg
}

func uploadConfigToCatalog(
	ctx workflow.Context,
	cfg *protos.FlowConnectionConfigs,
) {
	updateCtx := workflow.WithLocalActivityOptions(ctx, workflow.LocalActivityOptions{
		StartToCloseTimeout: 5 * time.Minute,
	})

	logger := workflow.GetLogger(ctx)
	updateFuture := workflow.ExecuteLocalActivity(updateCtx, updateCDCConfigInCatalogActivity, logger, cfg)
	if err := updateFuture.Get(updateCtx, nil); err != nil {
		logger.Warn("Failed to update CDC config in catalog", slog.Any("error", err))
	}
}

func processCDCFlowConfigUpdate(
	ctx workflow.Context,
	logger log.Logger,
	cfg *protos.FlowConnectionConfigs,
	state *CDCFlowWorkflowState,
	mirrorNameSearch temporal.SearchAttributes,
) error {
	flowConfigUpdate := state.FlowConfigUpdate

	// only modify for options since SyncFlow uses it
	if flowConfigUpdate.BatchSize > 0 {
		state.SyncFlowOptions.BatchSize = flowConfigUpdate.BatchSize
	}
	if flowConfigUpdate.IdleTimeout > 0 {
		state.SyncFlowOptions.IdleTimeoutSeconds = flowConfigUpdate.IdleTimeout
	}
	if flowConfigUpdate.NumberOfSyncs > 0 {
		state.SyncFlowOptions.NumberOfSyncs = flowConfigUpdate.NumberOfSyncs
	} else if flowConfigUpdate.NumberOfSyncs < 0 {
		state.SyncFlowOptions.NumberOfSyncs = 0
	}
	if flowConfigUpdate.UpdatedEnv != nil {
		if cfg.Env == nil {
			cfg.Env = make(map[string]string, len(flowConfigUpdate.UpdatedEnv))
		}
		maps.Copy(cfg.Env, flowConfigUpdate.UpdatedEnv)
	}
	if flowConfigUpdate.SnapshotNumRowsPerPartition > 0 {
		state.SnapshotNumRowsPerPartition = flowConfigUpdate.SnapshotNumRowsPerPartition
	}
	if flowConfigUpdate.SnapshotMaxParallelWorkers > 0 {
		state.SnapshotMaxParallelWorkers = flowConfigUpdate.SnapshotMaxParallelWorkers
	}
	if flowConfigUpdate.SnapshotNumTablesInParallel > 0 {
		state.SnapshotNumTablesInParallel = flowConfigUpdate.SnapshotNumTablesInParallel
	}

	tablesAreAdded := len(flowConfigUpdate.AdditionalTables) > 0
	tablesAreRemoved := len(flowConfigUpdate.RemovedTables) > 0
	if !tablesAreAdded && !tablesAreRemoved {
		syncStateToConfigProtoInCatalog(ctx, cfg, state)
		return nil
	}

	logger.Info("processing CDCFlowConfigUpdate", slog.Any("updatedState", flowConfigUpdate))

	if tablesAreAdded {
		if err := processTableAdditions(ctx, logger, cfg, state, mirrorNameSearch); err != nil {
			logger.Error("failed to process additional tables", slog.Any("error", err))
			return err
		}
	}

	if tablesAreRemoved {
		if err := processTableRemovals(ctx, logger, cfg, state); err != nil {
			logger.Error("failed to process removed tables", slog.Any("error", err))
			return err
		}
	}

	syncStateToConfigProtoInCatalog(ctx, cfg, state)
	return nil
}

func processTableAdditions(
	ctx workflow.Context,
	logger log.Logger,
	cfg *protos.FlowConnectionConfigs,
	state *CDCFlowWorkflowState,
	mirrorNameSearch temporal.SearchAttributes,
) error {
	flowConfigUpdate := state.FlowConfigUpdate
	if len(flowConfigUpdate.AdditionalTables) == 0 {
		syncStateToConfigProtoInCatalog(ctx, cfg, state)
		return nil
	}
	if internal.AdditionalTablesHasOverlap(state.SyncFlowOptions.TableMappings, flowConfigUpdate.AdditionalTables) {
		logger.Warn("duplicate source/destination tables found in additionalTables")
		syncStateToConfigProtoInCatalog(ctx, cfg, state)
		return nil
	}
	state.updateStatus(ctx, logger, protos.FlowStatus_STATUS_SNAPSHOT)

	logger.Info("altering publication for additional tables")
	alterPublicationAddAdditionalTablesCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 5 * time.Minute,
	})
	alterPublicationAddAdditionalTablesFuture := workflow.ExecuteActivity(
		alterPublicationAddAdditionalTablesCtx,
		flowable.AddTablesToPublication,
		cfg, flowConfigUpdate.AdditionalTables)
	if err := alterPublicationAddAdditionalTablesFuture.Get(ctx, nil); err != nil {
		logger.Error("failed to alter publication for additional tables", slog.Any("error", err))
		return err
	}

	logger.Info("additional tables added to publication")
	additionalTablesUUID := GetUUID(ctx)
	childAdditionalTablesCDCFlowID := GetChildWorkflowID("additional-cdc-flow", cfg.FlowJobName, additionalTablesUUID)
	additionalTablesCfg := proto.CloneOf(cfg)
	additionalTablesCfg.DoInitialSnapshot = true
	additionalTablesCfg.InitialSnapshotOnly = true
	additionalTablesCfg.TableMappings = flowConfigUpdate.AdditionalTables
	additionalTablesCfg.Resync = false
	additionalTablesCfg.SnapshotNumRowsPerPartition = state.SnapshotNumRowsPerPartition
	additionalTablesCfg.SnapshotMaxParallelWorkers = state.SnapshotMaxParallelWorkers
	additionalTablesCfg.SnapshotNumTablesInParallel = state.SnapshotNumTablesInParallel

	addTablesSelector := workflow.NewNamedSelector(ctx, "AddTables")
	addTablesSelector.AddReceive(ctx.Done(), func(_ workflow.ReceiveChannel, _ bool) {})
	flowSignalStateChangeChan := model.FlowSignalStateChange.GetSignalChannel(ctx)
	flowSignalStateChangeChan.AddToSelector(addTablesSelector, func(val *protos.FlowStateChangeRequest, _ bool) {
		if val.RequestedFlowState == protos.FlowStatus_STATUS_TERMINATING {
			logger.Info("terminating CDCFlow during table additions")
			state.ActiveSignal = model.TerminateSignal
			dropCfg := syncStateToConfigProtoInCatalog(ctx, cfg, state)
			state.DropFlowInput = &protos.DropFlowInput{
				FlowJobName:           dropCfg.FlowJobName,
				FlowConnectionConfigs: dropCfg,
				DropFlowStats:         val.DropMirrorStats,
				SkipDestinationDrop:   val.SkipDestinationDrop,
			}
		} else if val.RequestedFlowState == protos.FlowStatus_STATUS_RESYNC {
			logger.Info("resync requested during table additions")
			state.ActiveSignal = model.ResyncSignal
			// since we are adding to TableMappings, multiple signals can lead to duplicates
			// we should ContinueAsNew after the first signal in the selector, but just in case
			cfg.Resync = true
			cfg.DoInitialSnapshot = true
			state.DropFlowInput = &protos.DropFlowInput{
				// to be filled in just before ContinueAsNew
				FlowJobName:           "",
				FlowConnectionConfigs: nil,
				DropFlowStats:         val.DropMirrorStats,
				SkipDestinationDrop:   val.SkipDestinationDrop,
				Resync:                true,
			}
		} else if val.RequestedFlowState == protos.FlowStatus_STATUS_PAUSED {
			logger.Info("pause requested during table additions, ignoring")
		}
	})

	// execute the sync flow as a child workflow
	childAddTablesCDCFlowOpts := workflow.ChildWorkflowOptions{
		WorkflowID:        childAdditionalTablesCDCFlowID,
		ParentClosePolicy: enums.PARENT_CLOSE_POLICY_REQUEST_CANCEL,
		RetryPolicy: &temporal.RetryPolicy{
			MaximumAttempts: 20,
		},
		TypedSearchAttributes: mirrorNameSearch,
		WaitForCancellation:   true,
	}
	childAddTablesCDCFlowCtx := workflow.WithChildOptions(ctx, childAddTablesCDCFlowOpts)
	childAddTablesCDCFlowFuture := workflow.ExecuteChildWorkflow(
		childAddTablesCDCFlowCtx,
		CDCFlowWorkflow,
		additionalTablesCfg,
		nil,
	)
	var res *CDCFlowWorkflowResult
	var addTablesFlowErr error
	addTablesSelector.AddFuture(childAddTablesCDCFlowFuture, func(f workflow.Future) {
		addTablesFlowErr = f.Get(childAddTablesCDCFlowCtx, &res)
	})

	for res == nil {
		addTablesSelector.Select(ctx)
		if state.ActiveSignal == model.TerminateSignal || state.ActiveSignal == model.ResyncSignal {
			if state.ActiveSignal == model.ResyncSignal {
				// additional tables should also be resynced, we don't know how much was done so far
				state.SyncFlowOptions.TableMappings = append(state.SyncFlowOptions.TableMappings, flowConfigUpdate.AdditionalTables...)
				resyncCfg := syncStateToConfigProtoInCatalog(ctx, cfg, state)
				state.DropFlowInput.FlowJobName = resyncCfg.FlowJobName
				state.DropFlowInput.FlowConnectionConfigs = resyncCfg
			}
			return workflow.NewContinueAsNewError(ctx, DropFlowWorkflow, state.DropFlowInput)
		}
		if err := ctx.Err(); err != nil {
			logger.Info("CDCFlow canceled during table additions", slog.Any("error", err))
			return err
		}
		if addTablesFlowErr != nil {
			logger.Error("failed to execute child CDCFlow for additional tables", slog.Any("error", addTablesFlowErr))
			return fmt.Errorf("failed to execute child CDCFlow for additional tables: %w", addTablesFlowErr)
		}
	}

	maps.Copy(state.SyncFlowOptions.SrcTableIdNameMapping, res.SyncFlowOptions.SrcTableIdNameMapping)

	state.SyncFlowOptions.TableMappings = append(state.SyncFlowOptions.TableMappings, flowConfigUpdate.AdditionalTables...)
	logger.Info("additional tables added to sync flow")
	return nil
}

func processTableRemovals(
	ctx workflow.Context,
	logger log.Logger,
	cfg *protos.FlowConnectionConfigs,
	state *CDCFlowWorkflowState,
) error {
	logger.Info("altering publication for removed tables")
	removeTablesCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 5 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval: 1 * time.Minute,
		},
		WaitForCancellation: true,
	})
	alterPublicationRemovedTablesFuture := workflow.ExecuteActivity(
		removeTablesCtx,
		flowable.RemoveTablesFromPublication,
		cfg, state.FlowConfigUpdate.RemovedTables)
	if err := alterPublicationRemovedTablesFuture.Get(ctx, nil); err != nil {
		logger.Error("failed to alter publication for removed tables", slog.Any("error", err))
		return err
	}
	logger.Info("tables removed from publication")

	rawTableCleanupFuture := workflow.ExecuteActivity(
		removeTablesCtx,
		flowable.RemoveTablesFromRawTable,
		cfg, state.FlowConfigUpdate.RemovedTables)
	if err := rawTableCleanupFuture.Get(ctx, nil); err != nil {
		logger.Error("failed to clean up raw table for removed tables", slog.Any("error", err))
		return err
	}
	logger.Info("tables removed from raw table")

	removeTablesFromCatalogFuture := workflow.ExecuteActivity(
		removeTablesCtx,
		flowable.RemoveTablesFromCatalog,
		cfg, state.FlowConfigUpdate.RemovedTables)
	if err := removeTablesFromCatalogFuture.Get(ctx, nil); err != nil {
		logger.Error("failed to clean up raw table for removed tables", "error", err)
		return err
	}
	logger.Info("tables removed from catalog")

	// remove the tables from the sync flow options
	removedTables := make(map[string]struct{}, len(state.FlowConfigUpdate.RemovedTables))
	for _, removedTable := range state.FlowConfigUpdate.RemovedTables {
		removedTables[removedTable.SourceTableIdentifier] = struct{}{}
	}

	maps.DeleteFunc(state.SyncFlowOptions.SrcTableIdNameMapping, func(k uint32, v string) bool {
		_, removed := removedTables[v]
		return removed
	})
	state.SyncFlowOptions.TableMappings = slices.DeleteFunc(state.SyncFlowOptions.TableMappings, func(tm *protos.TableMapping) bool {
		_, removed := removedTables[tm.SourceTableIdentifier]
		return removed
	})

	return nil
}

func addCdcPropertiesSignalListener(
	ctx workflow.Context,
	logger log.Logger,
	selector workflow.Selector,
	state *CDCFlowWorkflowState,
) {
	cdcPropertiesSignalChan := model.CDCDynamicPropertiesSignal.GetSignalChannel(ctx)
	cdcPropertiesSignalChan.AddToSelector(selector, func(cdcConfigUpdate *protos.CDCFlowConfigUpdate, more bool) {
		// do this irrespective of additional tables being present, for auto unpausing
		state.FlowConfigUpdate = cdcConfigUpdate
		logger.Info("CDC Signal received",
			slog.Uint64("BatchSize", uint64(state.SyncFlowOptions.BatchSize)),
			slog.Uint64("IdleTimeout", state.SyncFlowOptions.IdleTimeoutSeconds),
			slog.Any("AdditionalTables", cdcConfigUpdate.AdditionalTables),
			slog.Any("RemovedTables", cdcConfigUpdate.RemovedTables),
			slog.Int64("NumberOfSyncs", int64(state.SyncFlowOptions.NumberOfSyncs)),
			slog.Any("UpdatedEnv", cdcConfigUpdate.UpdatedEnv),
			slog.Uint64("SnapshotNumRowsPerPartition", uint64(cdcConfigUpdate.SnapshotNumRowsPerPartition)),
			slog.Uint64("SnapshotMaxParallelWorkers", uint64(cdcConfigUpdate.SnapshotMaxParallelWorkers)),
			slog.Uint64("SnapshotNumTablesInParallel", uint64(cdcConfigUpdate.SnapshotNumTablesInParallel)),
		)
	})
}

func CDCFlowWorkflow(
	ctx workflow.Context,
	cfg *protos.FlowConnectionConfigs,
	state *CDCFlowWorkflowState,
) (*CDCFlowWorkflowResult, error) {
	if cfg == nil {
		return nil, errors.New("invalid connection configs")
	}

	logger := log.With(workflow.GetLogger(ctx), slog.String(string(shared.FlowNameKey), cfg.FlowJobName))
	if state == nil {
		state = NewCDCFlowWorkflowState(ctx, logger, cfg)
	}

	flowSignalChan := model.FlowSignal.GetSignalChannel(ctx)
	flowSignalStateChangeChan := model.FlowSignalStateChange.GetSignalChannel(ctx)
	if err := workflow.SetQueryHandler(ctx, shared.CDCFlowStateQuery, func() (CDCFlowWorkflowState, error) {
		return *state, nil
	}); err != nil {
		return state, fmt.Errorf("failed to set `%s` query handler: %w", shared.CDCFlowStateQuery, err)
	}
	if err := workflow.SetQueryHandler(ctx, shared.FlowStatusQuery, func() (protos.FlowStatus, error) {
		return state.CurrentFlowStatus, nil
	}); err != nil {
		return state, fmt.Errorf("failed to set `%s` query handler: %w", shared.FlowStatusQuery, err)
	}

	if state.CurrentFlowStatus == protos.FlowStatus_STATUS_COMPLETED {
		return state, nil
	}

	mirrorNameSearch := shared.NewSearchAttributes(cfg.FlowJobName)

	if state.ActiveSignal == model.PauseSignal {
		selector := workflow.NewNamedSelector(ctx, "PauseLoop")
		selector.AddReceive(ctx.Done(), func(_ workflow.ReceiveChannel, _ bool) {})
		flowSignalChan.AddToSelector(selector, func(val model.CDCFlowSignal, _ bool) {
			state.ActiveSignal = model.FlowSignalHandler(state.ActiveSignal, val, logger)
		})
		flowSignalStateChangeChan.AddToSelector(selector, func(val *protos.FlowStateChangeRequest, _ bool) {
			switch val.RequestedFlowState {
			case protos.FlowStatus_STATUS_TERMINATING:
				state.ActiveSignal = model.TerminateSignal
				dropCfg := syncStateToConfigProtoInCatalog(ctx, cfg, state)
				state.DropFlowInput = &protos.DropFlowInput{
					FlowJobName:           dropCfg.FlowJobName,
					FlowConnectionConfigs: dropCfg,
					DropFlowStats:         val.DropMirrorStats,
					SkipDestinationDrop:   val.SkipDestinationDrop,
				}
			case protos.FlowStatus_STATUS_RESYNC:
				state.ActiveSignal = model.ResyncSignal
				cfg.Resync = true
				cfg.DoInitialSnapshot = true
				resyncCfg := syncStateToConfigProtoInCatalog(ctx, cfg, state)
				state.DropFlowInput = &protos.DropFlowInput{
					FlowJobName:           resyncCfg.FlowJobName,
					FlowConnectionConfigs: resyncCfg,
					DropFlowStats:         val.DropMirrorStats,
					SkipDestinationDrop:   val.SkipDestinationDrop,
					Resync:                true,
				}
			}
		})
		addCdcPropertiesSignalListener(ctx, logger, selector, state)
		startTime := workflow.Now(ctx)
		state.updateStatus(ctx, logger, protos.FlowStatus_STATUS_PAUSED)

		for state.ActiveSignal == model.PauseSignal {
			// only place we block on receive, so signal processing is immediate
			for state.ActiveSignal == model.PauseSignal && state.FlowConfigUpdate == nil && ctx.Err() == nil {
				logger.Info(fmt.Sprintf("mirror has been paused for %s", time.Since(startTime).Round(time.Second)))
				selector.Select(ctx)
			}
			if err := ctx.Err(); err != nil {
				return state, err
			}
			if state.ActiveSignal == model.TerminateSignal || state.ActiveSignal == model.ResyncSignal {
				return state, workflow.NewContinueAsNewError(ctx, DropFlowWorkflow, state.DropFlowInput)
			}

			if state.FlowConfigUpdate != nil {
				if err := processCDCFlowConfigUpdate(ctx, logger, cfg, state, mirrorNameSearch); err != nil {
					return state, err
				}
				logger.Info("wiping flow state after state update processing")
				// finished processing, wipe it
				state.FlowConfigUpdate = nil
				state.ActiveSignal = model.NoopSignal
			}
		}

		logger.Info(fmt.Sprintf("mirror has been resumed after %s", time.Since(startTime).Round(time.Second)))
		state.updateStatus(ctx, logger, protos.FlowStatus_STATUS_RUNNING)
		return state, workflow.NewContinueAsNewError(ctx, CDCFlowWorkflow, cfg, state)
	}

	originalRunID := workflow.GetInfo(ctx).OriginalRunID

	var err error
	ctx, err = GetFlowMetadataContext(ctx, &protos.FlowContextMetadataInput{
		FlowName:        cfg.FlowJobName,
		SourceName:      cfg.SourceName,
		DestinationName: cfg.DestinationName,
		Status:          state.CurrentFlowStatus,
		IsResync:        cfg.Resync,
	})
	if err != nil {
		return state, fmt.Errorf("failed to get flow metadata context: %w", err)
	}

	// we cannot skip SetupFlow if SnapshotFlow did not complete in cases where Resync is enabled
	// because Resync modifies TableMappings before Setup and also before Snapshot
	// for safety, rely on the idempotency of SetupFlow instead
	// also, no signals are being handled until the loop starts, so no PAUSE/DROP will take here.
	if state.CurrentFlowStatus != protos.FlowStatus_STATUS_RUNNING {
		originalTableMappings := make([]*protos.TableMapping, 0, len(cfg.TableMappings))
		for _, tableMapping := range cfg.TableMappings {
			originalTableMappings = append(originalTableMappings, proto.CloneOf(tableMapping))
		}
		// if resync is true, alter the table name schema mapping to temporarily add
		// a suffix to the table names.
		if cfg.Resync {
			for _, mapping := range state.SyncFlowOptions.TableMappings {
				if mapping.Engine != protos.TableEngine_CH_ENGINE_NULL {
					mapping.DestinationTableIdentifier += "_resync"
				}
			}
			// because we have renamed the tables.
			cfg.TableMappings = state.SyncFlowOptions.TableMappings
		}

		// start the SetupFlow workflow as a child workflow, and wait for it to complete
		// it should return the table schema for the source peer
		setupFlowID := GetChildWorkflowID("setup-flow", cfg.FlowJobName, originalRunID)

		setupSnapshotSelector := workflow.NewNamedSelector(ctx, "Setup/Snapshot")
		setupSnapshotSelector.AddReceive(ctx.Done(), func(_ workflow.ReceiveChannel, _ bool) {})
		flowSignalStateChangeChan.AddToSelector(setupSnapshotSelector, func(val *protos.FlowStateChangeRequest, _ bool) {
			switch val.RequestedFlowState {
			case protos.FlowStatus_STATUS_PAUSED:
				logger.Warn("pause requested during setup, ignoring")
			case protos.FlowStatus_STATUS_TERMINATING:
				state.ActiveSignal = model.TerminateSignal
				dropCfg := syncStateToConfigProtoInCatalog(ctx, cfg, state)
				state.DropFlowInput = &protos.DropFlowInput{
					FlowJobName:           dropCfg.FlowJobName,
					FlowConnectionConfigs: dropCfg,
					DropFlowStats:         val.DropMirrorStats,
					SkipDestinationDrop:   val.SkipDestinationDrop,
				}
			case protos.FlowStatus_STATUS_RESYNC:
				state.ActiveSignal = model.ResyncSignal
				cfg.Resync = true
				cfg.DoInitialSnapshot = true
				cfg.TableMappings = originalTableMappings
				// this is the only place where we can have a resync during a resync
				// so we need to NOT sync the tableMappings to catalog to preserve original names
				uploadConfigToCatalog(ctx, cfg)
				state.DropFlowInput = &protos.DropFlowInput{
					FlowJobName:           cfg.FlowJobName,
					FlowConnectionConfigs: cfg,
					DropFlowStats:         val.DropMirrorStats,
					SkipDestinationDrop:   val.SkipDestinationDrop,
					Resync:                true,
				}
			}
		})

		childSetupFlowOpts := workflow.ChildWorkflowOptions{
			WorkflowID:        setupFlowID,
			ParentClosePolicy: enums.PARENT_CLOSE_POLICY_REQUEST_CANCEL,
			RetryPolicy: &temporal.RetryPolicy{
				MaximumAttempts: 20,
			},
			TypedSearchAttributes: mirrorNameSearch,
			WaitForCancellation:   true,
		}
		setupFlowCtx := workflow.WithChildOptions(ctx, childSetupFlowOpts)
		setupFlowFuture := workflow.ExecuteChildWorkflow(setupFlowCtx, SetupFlowWorkflow, cfg)

		var setupFlowOutput *protos.SetupFlowOutput
		var setupFlowError error
		setupSnapshotSelector.AddFuture(setupFlowFuture, func(f workflow.Future) {
			setupFlowError = f.Get(setupFlowCtx, &setupFlowOutput)
		})

		for setupFlowOutput == nil {
			setupSnapshotSelector.Select(ctx)
			if state.ActiveSignal == model.TerminateSignal || state.ActiveSignal == model.ResyncSignal {
				return state, workflow.NewContinueAsNewError(ctx, DropFlowWorkflow, state.DropFlowInput)
			}
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if setupFlowError != nil {
				return state, fmt.Errorf("failed to execute setup workflow: %w", setupFlowError)
			}
		}

		state.SyncFlowOptions.SrcTableIdNameMapping = setupFlowOutput.SrcTableIdNameMapping
		state.updateStatus(ctx, logger, protos.FlowStatus_STATUS_SNAPSHOT)

		// next part of the setup is to snapshot-initial-copy and setup replication slots.
		snapshotFlowID := GetChildWorkflowID("snapshot-flow", cfg.FlowJobName, originalRunID)

		taskQueue := internal.PeerFlowTaskQueueName(shared.SnapshotFlowTaskQueue)
		childSnapshotFlowOpts := workflow.ChildWorkflowOptions{
			WorkflowID:        snapshotFlowID,
			ParentClosePolicy: enums.PARENT_CLOSE_POLICY_REQUEST_CANCEL,
			RetryPolicy: &temporal.RetryPolicy{
				MaximumAttempts: 20,
			},
			TaskQueue:             taskQueue,
			TypedSearchAttributes: mirrorNameSearch,
			WaitForCancellation:   true,
		}

		snapshotFlowCtx := workflow.WithChildOptions(ctx, childSnapshotFlowOpts)
		// now snapshot parameters are also part of the state, but until we finish snapshot they wouldn't be modifiable.
		// so we can use the same cfg for snapshot flow, and then rely on being state being saved to catalog
		// during any operation that triggers another snapshot (INCLUDING add tables).
		// this could fail for very weird Temporal resets
		snapshotFlowFuture := workflow.ExecuteChildWorkflow(snapshotFlowCtx, SnapshotFlowWorkflow, cfg)
		var snapshotDone bool
		var snapshotError error
		setupSnapshotSelector.AddFuture(snapshotFlowFuture, func(f workflow.Future) {
			snapshotError = f.Get(snapshotFlowCtx, nil)
			snapshotDone = true
		})

		for !snapshotDone {
			setupSnapshotSelector.Select(ctx)
			if state.ActiveSignal == model.TerminateSignal || state.ActiveSignal == model.ResyncSignal {
				return state, workflow.NewContinueAsNewError(ctx, DropFlowWorkflow, state.DropFlowInput)
			}
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if snapshotError != nil {
				return state, fmt.Errorf("failed to execute snapshot workflow: %w", snapshotError)
			}
		}

		if cfg.Resync {
			renameOpts := &protos.RenameTablesInput{
				FlowJobName:       cfg.FlowJobName,
				PeerName:          cfg.DestinationName,
				SyncedAtColName:   cfg.SyncedAtColName,
				SoftDeleteColName: cfg.SoftDeleteColName,
			}

			for _, mapping := range state.SyncFlowOptions.TableMappings {
				if mapping.Engine != protos.TableEngine_CH_ENGINE_NULL {
					oldName := mapping.DestinationTableIdentifier
					newName := strings.TrimSuffix(oldName, "_resync")
					renameOpts.RenameTableOptions = append(renameOpts.RenameTableOptions, &protos.RenameTableOption{
						CurrentName: oldName,
						NewName:     newName,
					})
					mapping.DestinationTableIdentifier = newName
				} else {
					renameOpts.RenameTableOptions = append(renameOpts.RenameTableOptions, &protos.RenameTableOption{
						CurrentName: mapping.DestinationTableIdentifier,
						NewName:     mapping.DestinationTableIdentifier,
					})
				}
			}

			renameTablesCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
				StartToCloseTimeout: 12 * time.Hour,
				HeartbeatTimeout:    time.Minute,
				RetryPolicy: &temporal.RetryPolicy{
					InitialInterval: 1 * time.Minute,
				},
			})
			renameTablesFuture := workflow.ExecuteActivity(renameTablesCtx, flowable.RenameTables, renameOpts)
			var renameTablesDone bool
			var renameTablesError error
			setupSnapshotSelector.AddFuture(renameTablesFuture, func(f workflow.Future) {
				renameTablesDone = true
				if err := f.Get(renameTablesCtx, nil); err != nil {
					renameTablesError = fmt.Errorf("failed to execute rename tables activity: %w", err)
					logger.Error("failed to execute rename tables activity", slog.Any("error", err))
				} else {
					logger.Info("rename tables activity completed successfully")
				}
			})
			for !renameTablesDone {
				setupSnapshotSelector.Select(ctx)
				if state.ActiveSignal == model.TerminateSignal || state.ActiveSignal == model.ResyncSignal {
					return state, workflow.NewContinueAsNewError(ctx, DropFlowWorkflow, state.DropFlowInput)
				}
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				if renameTablesError != nil {
					return state, renameTablesError
				}
			}
		}

		// if initial_copy_only is opted for, we end the flow here.
		if cfg.InitialSnapshotOnly {
			logger.Info("initial snapshot only, ending flow")
			state.updateStatus(ctx, logger, protos.FlowStatus_STATUS_COMPLETED)
		} else {
			logger.Info("executed setup flow and snapshot flow, start running")
			state.updateStatus(ctx, logger, protos.FlowStatus_STATUS_RUNNING)
		}
		return state, workflow.NewContinueAsNewError(ctx, CDCFlowWorkflow, cfg, state)
	}

	var finished bool
	var finishedError bool
	syncCtx, cancelSync := workflow.WithCancel(workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 365 * 24 * time.Hour,
		HeartbeatTimeout:    time.Minute,
		WaitForCancellation: true,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 1},
	}))
	syncFlowFuture := workflow.ExecuteActivity(syncCtx, flowable.SyncFlow, cfg, state.SyncFlowOptions)

	mainLoopSelector := workflow.NewNamedSelector(ctx, "MainLoop")
	mainLoopSelector.AddReceive(ctx.Done(), func(_ workflow.ReceiveChannel, _ bool) {
		finished = true
	})
	mainLoopSelector.AddFuture(syncFlowFuture, func(f workflow.Future) {
		if err := f.Get(ctx, nil); err != nil {
			if finished || err == workflow.ErrCanceled {
				logger.Error("error in sync flow, but cdc finished", slog.Any("error", err))
				return
			}

			now := workflow.Now(ctx)
			if state.LastError.Add(24 * time.Hour).Before(now) {
				state.ErrorCount = 0
			}
			state.LastError = now
			var sleepFor time.Duration
			var panicErr *temporal.PanicError
			if errors.As(err, &panicErr) {
				sleepFor = time.Duration(10+min(state.ErrorCount, 3)*15) * time.Minute
				logger.Error(
					"panic in sync flow",
					slog.Any("error", panicErr.Error()),
					slog.String("stack", panicErr.StackTrace()),
					slog.Any("sleepFor", sleepFor),
				)
			} else {
				// cannot use shared.IsSQLStateError because temporal serialize/deserialize
				if !temporal.IsApplicationError(err) || strings.Contains(err.Error(), "(SQLSTATE 55006)") {
					sleepFor = time.Duration(1+min(state.ErrorCount, 9)) * time.Minute
				} else {
					sleepFor = time.Duration(5+min(state.ErrorCount, 5)*15) * time.Minute
				}

				logger.Error("error in sync flow", slog.Any("error", err), slog.Any("sleepFor", sleepFor))
			}
			mainLoopSelector.AddFuture(model.SleepFuture(ctx, sleepFor), func(_ workflow.Future) {
				logger.Info("sync finished after waiting after error")
				finished = true
				finishedError = true
				if state.SyncFlowOptions.NumberOfSyncs > 0 {
					state.ActiveSignal = model.PauseSignal
				}
			})
		} else {
			logger.Info("sync finished")
			finished = true
			if state.SyncFlowOptions.NumberOfSyncs > 0 {
				state.ActiveSignal = model.PauseSignal
			}
		}
	})

	flowSignalChan.AddToSelector(mainLoopSelector, func(val model.CDCFlowSignal, _ bool) {
		state.ActiveSignal = model.FlowSignalHandler(state.ActiveSignal, val, logger)
		if state.ActiveSignal == model.PauseSignal {
			finished = true
		}
	})
	flowSignalStateChangeChan.AddToSelector(mainLoopSelector, func(val *protos.FlowStateChangeRequest, _ bool) {
		finished = true
		switch val.RequestedFlowState {
		case protos.FlowStatus_STATUS_TERMINATING:
			state.ActiveSignal = model.TerminateSignal
			dropCfg := syncStateToConfigProtoInCatalog(ctx, cfg, state)
			state.DropFlowInput = &protos.DropFlowInput{
				FlowJobName:           dropCfg.FlowJobName,
				FlowConnectionConfigs: dropCfg,
				DropFlowStats:         val.DropMirrorStats,
				SkipDestinationDrop:   val.SkipDestinationDrop,
			}
		case protos.FlowStatus_STATUS_RESYNC:
			state.ActiveSignal = model.ResyncSignal
			cfg.Resync = true
			cfg.DoInitialSnapshot = true
			resyncCfg := syncStateToConfigProtoInCatalog(ctx, cfg, state)
			state.DropFlowInput = &protos.DropFlowInput{
				FlowJobName:           resyncCfg.FlowJobName,
				FlowConnectionConfigs: resyncCfg,
				DropFlowStats:         val.DropMirrorStats,
				SkipDestinationDrop:   val.SkipDestinationDrop,
				Resync:                true,
			}
		}
	})

	addCdcPropertiesSignalListener(ctx, logger, mainLoopSelector, state)

	state.updateStatus(ctx, logger, protos.FlowStatus_STATUS_RUNNING)
	for {
		mainLoopSelector.Select(ctx)
		for ctx.Err() == nil && mainLoopSelector.HasPending() {
			mainLoopSelector.Select(ctx)
		}
		if err := ctx.Err(); err != nil {
			logger.Info("mirror canceled", slog.Any("error", err))
			return state, err
		}

		if ShouldWorkflowContinueAsNew(ctx) {
			finished = true
		}

		if finished {
			// wait on sync flow before draining selector
			cancelSync()
			_ = syncFlowFuture.Get(ctx, nil)

			for ctx.Err() == nil && mainLoopSelector.HasPending() {
				mainLoopSelector.Select(ctx)
			}

			if err := ctx.Err(); err != nil {
				logger.Info("mirror canceled", slog.Any("error", err))
				state.updateStatus(ctx, logger, protos.FlowStatus_STATUS_TERMINATED)
				return nil, err
			}

			if finishedError {
				state.ErrorCount += 1
			} else {
				state.ErrorCount = 0
			}

			if state.ActiveSignal == model.TerminateSignal || state.ActiveSignal == model.ResyncSignal {
				return state, workflow.NewContinueAsNewError(ctx, DropFlowWorkflow, state.DropFlowInput)
			}
			return state, workflow.NewContinueAsNewError(ctx, CDCFlowWorkflow, cfg, state)
		}
	}
}
