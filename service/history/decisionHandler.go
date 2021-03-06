// Copyright (c) 2017 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package history

import (
	ctx "context"
	"fmt"
	"time"

	"go.uber.org/yarpc"

	"github.com/uber/cadence/common"
	"github.com/uber/cadence/common/cache"
	"github.com/uber/cadence/common/client"
	"github.com/uber/cadence/common/clock"
	"github.com/uber/cadence/common/log"
	"github.com/uber/cadence/common/log/tag"
	"github.com/uber/cadence/common/metrics"
	"github.com/uber/cadence/common/persistence"
	"github.com/uber/cadence/common/types"
	"github.com/uber/cadence/service/history/config"
	"github.com/uber/cadence/service/history/execution"
	"github.com/uber/cadence/service/history/query"
	"github.com/uber/cadence/service/history/queue"
	"github.com/uber/cadence/service/history/shard"
)

type (
	// decision business logic handler
	decisionHandler interface {
		handleDecisionTaskScheduled(ctx.Context, *types.ScheduleDecisionTaskRequest) error
		handleDecisionTaskStarted(ctx.Context,
			*types.RecordDecisionTaskStartedRequest) (*types.RecordDecisionTaskStartedResponse, error)
		handleDecisionTaskFailed(ctx.Context,
			*types.HistoryRespondDecisionTaskFailedRequest) error
		handleDecisionTaskCompleted(ctx.Context,
			*types.HistoryRespondDecisionTaskCompletedRequest) (*types.HistoryRespondDecisionTaskCompletedResponse, error)
		// TODO also include the handle of decision timeout here
	}

	decisionHandlerImpl struct {
		currentClusterName    string
		config                *config.Config
		shard                 shard.Context
		timeSource            clock.TimeSource
		historyEngine         *historyEngineImpl
		domainCache           cache.DomainCache
		executionCache        *execution.Cache
		txProcessor           queue.Processor
		timerProcessor        queue.Processor
		tokenSerializer       common.TaskTokenSerializer
		metricsClient         metrics.Client
		logger                log.Logger
		throttledLogger       log.Logger
		decisionAttrValidator *decisionAttrValidator
		versionChecker        client.VersionChecker
	}
)

func newDecisionHandler(historyEngine *historyEngineImpl) *decisionHandlerImpl {
	return &decisionHandlerImpl{
		currentClusterName: historyEngine.currentClusterName,
		config:             historyEngine.config,
		shard:              historyEngine.shard,
		timeSource:         historyEngine.shard.GetTimeSource(),
		historyEngine:      historyEngine,
		domainCache:        historyEngine.shard.GetDomainCache(),
		executionCache:     historyEngine.executionCache,
		txProcessor:        historyEngine.txProcessor,
		timerProcessor:     historyEngine.timerProcessor,
		tokenSerializer:    historyEngine.tokenSerializer,
		metricsClient:      historyEngine.metricsClient,
		logger:             historyEngine.logger,
		throttledLogger:    historyEngine.throttledLogger,
		decisionAttrValidator: newDecisionAttrValidator(
			historyEngine.shard.GetDomainCache(),
			historyEngine.config,
			historyEngine.logger,
		),
		versionChecker: client.NewVersionChecker(),
	}
}

func (handler *decisionHandlerImpl) handleDecisionTaskScheduled(
	ctx ctx.Context,
	req *types.ScheduleDecisionTaskRequest,
) error {

	domainEntry, err := handler.historyEngine.getActiveDomainEntry(req.DomainUUID)
	if err != nil {
		return err
	}
	domainID := domainEntry.GetInfo().ID

	workflowExecution := types.WorkflowExecution{
		WorkflowID: req.WorkflowExecution.WorkflowID,
		RunID:      req.WorkflowExecution.RunID,
	}

	return handler.historyEngine.updateWorkflowExecutionWithAction(ctx, domainID, workflowExecution,
		func(context execution.Context, mutableState execution.MutableState) (*updateWorkflowAction, error) {
			if !mutableState.IsWorkflowExecutionRunning() {
				return nil, ErrWorkflowCompleted
			}

			if mutableState.HasProcessedOrPendingDecision() {
				return &updateWorkflowAction{
					noop: true,
				}, nil
			}

			startEvent, err := mutableState.GetStartEvent(ctx)
			if err != nil {
				return nil, err
			}
			if err := mutableState.AddFirstDecisionTaskScheduled(
				startEvent,
			); err != nil {
				return nil, err
			}

			return &updateWorkflowAction{}, nil
		})
}

func (handler *decisionHandlerImpl) handleDecisionTaskStarted(
	ctx ctx.Context,
	req *types.RecordDecisionTaskStartedRequest,
) (*types.RecordDecisionTaskStartedResponse, error) {

	domainEntry, err := handler.historyEngine.getActiveDomainEntry(req.DomainUUID)
	if err != nil {
		return nil, err
	}
	domainID := domainEntry.GetInfo().ID

	workflowExecution := types.WorkflowExecution{
		WorkflowID: req.WorkflowExecution.WorkflowID,
		RunID:      req.WorkflowExecution.RunID,
	}

	scheduleID := req.GetScheduleID()
	requestID := req.GetRequestID()

	var resp *types.RecordDecisionTaskStartedResponse
	err = handler.historyEngine.updateWorkflowExecutionWithAction(ctx, domainID, workflowExecution,
		func(context execution.Context, mutableState execution.MutableState) (*updateWorkflowAction, error) {
			if !mutableState.IsWorkflowExecutionRunning() {
				return nil, ErrWorkflowCompleted
			}

			decision, isRunning := mutableState.GetDecisionInfo(scheduleID)

			// First check to see if cache needs to be refreshed as we could potentially have stale workflow execution in
			// some extreme cassandra failure cases.
			if !isRunning && scheduleID >= mutableState.GetNextEventID() {
				handler.metricsClient.IncCounter(metrics.HistoryRecordDecisionTaskStartedScope, metrics.StaleMutableStateCounter)
				// Reload workflow execution history
				// ErrStaleState will trigger updateWorkflowExecutionWithAction function to reload the mutable state
				return nil, ErrStaleState
			}

			// Check execution state to make sure task is in the list of outstanding tasks and it is not yet started.  If
			// task is not outstanding than it is most probably a duplicate and complete the task.
			if !isRunning {
				// Looks like DecisionTask already completed as a result of another call.
				// It is OK to drop the task at this point.
				return nil, &types.EntityNotExistsError{Message: "Decision task not found."}
			}

			updateAction := &updateWorkflowAction{}

			if decision.StartedID != common.EmptyEventID {
				// If decision is started as part of the current request scope then return a positive response
				if decision.RequestID == requestID {
					resp, err = handler.createRecordDecisionTaskStartedResponse(domainID, mutableState, decision, req.PollRequest.GetIdentity())
					if err != nil {
						return nil, err
					}
					updateAction.noop = true
					return updateAction, nil
				}

				// Looks like DecisionTask already started as a result of another call.
				// It is OK to drop the task at this point.
				return nil, &types.EventAlreadyStartedError{Message: "Decision task already started."}
			}

			_, decision, err = mutableState.AddDecisionTaskStartedEvent(scheduleID, requestID, req.PollRequest)
			if err != nil {
				// Unable to add DecisionTaskStarted event to history
				return nil, &types.InternalServiceError{Message: "Unable to add DecisionTaskStarted event to history."}
			}

			resp, err = handler.createRecordDecisionTaskStartedResponse(domainID, mutableState, decision, req.PollRequest.GetIdentity())
			if err != nil {
				return nil, err
			}
			return updateAction, nil
		})

	if err != nil {
		return nil, err
	}
	return resp, nil
}

func (handler *decisionHandlerImpl) handleDecisionTaskFailed(
	ctx ctx.Context,
	req *types.HistoryRespondDecisionTaskFailedRequest,
) (retError error) {

	domainEntry, err := handler.historyEngine.getActiveDomainEntry(req.DomainUUID)
	if err != nil {
		return err
	}
	domainID := domainEntry.GetInfo().ID

	request := req.FailedRequest
	token, err := handler.tokenSerializer.Deserialize(request.TaskToken)
	if err != nil {
		return ErrDeserializingToken
	}

	workflowExecution := types.WorkflowExecution{
		WorkflowID: common.StringPtr(token.WorkflowID),
		RunID:      common.StringPtr(token.RunID),
	}

	return handler.historyEngine.updateWorkflowExecution(ctx, domainID, workflowExecution, true,
		func(context execution.Context, mutableState execution.MutableState) error {
			if !mutableState.IsWorkflowExecutionRunning() {
				return ErrWorkflowCompleted
			}

			scheduleID := token.ScheduleID
			decision, isRunning := mutableState.GetDecisionInfo(scheduleID)
			if !isRunning || decision.Attempt != token.ScheduleAttempt || decision.StartedID == common.EmptyEventID {
				return &types.EntityNotExistsError{Message: "Decision task not found."}
			}

			_, err := mutableState.AddDecisionTaskFailedEvent(decision.ScheduleID, decision.StartedID, request.GetCause(), request.Details,
				request.GetIdentity(), "", request.GetBinaryChecksum(), "", "", 0)
			return err
		})
}

func (handler *decisionHandlerImpl) handleDecisionTaskCompleted(
	ctx ctx.Context,
	req *types.HistoryRespondDecisionTaskCompletedRequest,
) (resp *types.HistoryRespondDecisionTaskCompletedResponse, retError error) {

	domainEntry, err := handler.historyEngine.getActiveDomainEntry(req.DomainUUID)
	if err != nil {
		return nil, err
	}
	domainID := domainEntry.GetInfo().ID

	request := req.CompleteRequest
	token, err0 := handler.tokenSerializer.Deserialize(request.TaskToken)
	if err0 != nil {
		return nil, ErrDeserializingToken
	}

	workflowExecution := types.WorkflowExecution{
		WorkflowID: common.StringPtr(token.WorkflowID),
		RunID:      common.StringPtr(token.RunID),
	}

	call := yarpc.CallFromContext(ctx)
	clientLibVersion := call.Header(common.LibraryVersionHeaderName)
	clientFeatureVersion := call.Header(common.FeatureVersionHeaderName)
	clientImpl := call.Header(common.ClientImplHeaderName)

	wfContext, release, err := handler.executionCache.GetOrCreateWorkflowExecution(ctx, domainID, workflowExecution)
	if err != nil {
		return nil, err
	}
	defer func() { release(retError) }()

Update_History_Loop:
	for attempt := 0; attempt < conditionalRetryCount; attempt++ {
		msBuilder, err := wfContext.LoadWorkflowExecution(ctx)
		if err != nil {
			return nil, err
		}
		if !msBuilder.IsWorkflowExecutionRunning() {
			return nil, ErrWorkflowCompleted
		}
		executionStats, err := wfContext.LoadExecutionStats(ctx)
		if err != nil {
			return nil, err
		}

		executionInfo := msBuilder.GetExecutionInfo()

		scheduleID := token.ScheduleID
		currentDecision, isRunning := msBuilder.GetDecisionInfo(scheduleID)

		// First check to see if cache needs to be refreshed as we could potentially have stale workflow execution in
		// some extreme cassandra failure cases.
		if !isRunning && scheduleID >= msBuilder.GetNextEventID() {
			handler.metricsClient.IncCounter(metrics.HistoryRespondDecisionTaskCompletedScope, metrics.StaleMutableStateCounter)
			// Reload workflow execution history
			wfContext.Clear()
			continue Update_History_Loop
		}

		if !msBuilder.IsWorkflowExecutionRunning() || !isRunning || currentDecision.Attempt != token.ScheduleAttempt ||
			currentDecision.StartedID == common.EmptyEventID {
			return nil, &types.EntityNotExistsError{Message: "Decision task not found."}
		}

		startedID := currentDecision.StartedID
		maxResetPoints := handler.config.MaxAutoResetPoints(domainEntry.GetInfo().Name)
		if msBuilder.GetExecutionInfo().AutoResetPoints != nil && maxResetPoints == len(msBuilder.GetExecutionInfo().AutoResetPoints.Points) {
			handler.metricsClient.IncCounter(metrics.HistoryRespondDecisionTaskCompletedScope, metrics.AutoResetPointsLimitExceededCounter)
		}

		decisionHeartbeating := request.GetForceCreateNewDecisionTask() && len(request.Decisions) == 0
		var decisionHeartbeatTimeout bool
		var completedEvent *types.HistoryEvent
		if decisionHeartbeating {
			domainName := domainEntry.GetInfo().Name
			timeout := handler.config.DecisionHeartbeatTimeout(domainName)
			if currentDecision.OriginalScheduledTimestamp > 0 && handler.timeSource.Now().After(time.Unix(0, currentDecision.OriginalScheduledTimestamp).Add(timeout)) {
				decisionHeartbeatTimeout = true
				scope := handler.metricsClient.Scope(metrics.HistoryRespondDecisionTaskCompletedScope, metrics.DomainTag(domainName))
				scope.IncCounter(metrics.DecisionHeartbeatTimeoutCounter)
				completedEvent, err = msBuilder.AddDecisionTaskTimedOutEvent(currentDecision.ScheduleID, currentDecision.StartedID)
				if err != nil {
					return nil, &types.InternalServiceError{Message: "Failed to add decision timeout event."}
				}
				msBuilder.ClearStickyness()
			} else {
				completedEvent, err = msBuilder.AddDecisionTaskCompletedEvent(scheduleID, startedID, request, maxResetPoints)
				if err != nil {
					return nil, &types.InternalServiceError{Message: "Unable to add DecisionTaskCompleted event to history."}
				}
			}
		} else {
			completedEvent, err = msBuilder.AddDecisionTaskCompletedEvent(scheduleID, startedID, request, maxResetPoints)
			if err != nil {
				return nil, &types.InternalServiceError{Message: "Unable to add DecisionTaskCompleted event to history."}
			}
		}

		var (
			failDecision                bool
			failCause                   types.DecisionTaskFailedCause
			failMessage                 string
			activityNotStartedCancelled bool
			continueAsNewBuilder        execution.MutableState
			hasUnhandledEvents          bool
			decisionResults             []*decisionResult
		)
		hasUnhandledEvents = msBuilder.HasBufferedEvents()

		if request.StickyAttributes == nil || request.StickyAttributes.WorkerTaskList == nil {
			handler.metricsClient.IncCounter(metrics.HistoryRespondDecisionTaskCompletedScope, metrics.CompleteDecisionWithStickyDisabledCounter)
			executionInfo.StickyTaskList = ""
			executionInfo.StickyScheduleToStartTimeout = 0
		} else {
			handler.metricsClient.IncCounter(metrics.HistoryRespondDecisionTaskCompletedScope, metrics.CompleteDecisionWithStickyEnabledCounter)
			executionInfo.StickyTaskList = request.StickyAttributes.WorkerTaskList.GetName()
			executionInfo.StickyScheduleToStartTimeout = request.StickyAttributes.GetScheduleToStartTimeoutSeconds()
		}
		executionInfo.ClientLibraryVersion = clientLibVersion
		executionInfo.ClientFeatureVersion = clientFeatureVersion
		executionInfo.ClientImpl = clientImpl

		binChecksum := request.GetBinaryChecksum()
		if _, ok := domainEntry.GetConfig().BadBinaries.Binaries[binChecksum]; ok {
			failDecision = true
			failCause = types.DecisionTaskFailedCauseBadBinary
			failMessage = fmt.Sprintf("binary %v is already marked as bad deployment", binChecksum)
		} else {

			domainName := domainEntry.GetInfo().Name
			workflowSizeChecker := newWorkflowSizeChecker(
				handler.config.BlobSizeLimitWarn(domainName),
				handler.config.BlobSizeLimitError(domainName),
				handler.config.HistorySizeLimitWarn(domainName),
				handler.config.HistorySizeLimitError(domainName),
				handler.config.HistoryCountLimitWarn(domainName),
				handler.config.HistoryCountLimitError(domainName),
				completedEvent.GetEventID(),
				msBuilder,
				executionStats,
				handler.metricsClient.Scope(metrics.HistoryRespondDecisionTaskCompletedScope, metrics.DomainTag(domainName)),
				handler.throttledLogger,
			)

			decisionTaskHandler := newDecisionTaskHandler(
				request.GetIdentity(),
				completedEvent.GetEventID(),
				domainEntry,
				msBuilder,
				handler.decisionAttrValidator,
				workflowSizeChecker,
				handler.tokenSerializer,
				handler.logger,
				handler.domainCache,
				handler.metricsClient,
				handler.config,
			)

			if decisionResults, err = decisionTaskHandler.handleDecisions(
				ctx,
				request.ExecutionContext,
				request.Decisions,
			); err != nil {
				return nil, err
			}

			// set the vars used by following logic
			// further refactor should also clean up the vars used below
			failDecision = decisionTaskHandler.failDecision
			if failDecision {
				failCause = *decisionTaskHandler.failDecisionCause
				failMessage = *decisionTaskHandler.failMessage
			}

			// failMessage is not used by decisionTaskHandler
			activityNotStartedCancelled = decisionTaskHandler.activityNotStartedCancelled
			// continueAsNewTimerTasks is not used by decisionTaskHandler

			continueAsNewBuilder = decisionTaskHandler.continueAsNewBuilder

			hasUnhandledEvents = decisionTaskHandler.hasUnhandledEventsBeforeDecisions
		}

		if failDecision {
			handler.metricsClient.IncCounter(metrics.HistoryRespondDecisionTaskCompletedScope, metrics.FailedDecisionsCounter)
			handler.logger.Info("Failing the decision.", tag.WorkflowDecisionFailCause(int64(failCause)),
				tag.WorkflowID(token.WorkflowID),
				tag.WorkflowRunID(token.RunID),
				tag.WorkflowDomainID(domainID))
			msBuilder, err = handler.historyEngine.failDecision(ctx, wfContext, scheduleID, startedID, failCause, []byte(failMessage), request)
			if err != nil {
				return nil, err
			}
			hasUnhandledEvents = true
			continueAsNewBuilder = nil
		}

		createNewDecisionTask := msBuilder.IsWorkflowExecutionRunning() && (hasUnhandledEvents || request.GetForceCreateNewDecisionTask() || activityNotStartedCancelled)
		var newDecisionTaskScheduledID int64
		if createNewDecisionTask {
			var newDecision *execution.DecisionInfo
			var err error
			if decisionHeartbeating && !decisionHeartbeatTimeout {
				newDecision, err = msBuilder.AddDecisionTaskScheduledEventAsHeartbeat(
					request.GetReturnNewDecisionTask(),
					currentDecision.OriginalScheduledTimestamp,
				)
			} else {
				newDecision, err = msBuilder.AddDecisionTaskScheduledEvent(
					request.GetReturnNewDecisionTask(),
				)
			}
			if err != nil {
				return nil, &types.InternalServiceError{Message: "Failed to add decision scheduled event."}
			}

			newDecisionTaskScheduledID = newDecision.ScheduleID
			// skip transfer task for decision if request asking to return new decision task
			if request.GetReturnNewDecisionTask() {
				// start the new decision task if request asked to do so
				// TODO: replace the poll request
				_, _, err := msBuilder.AddDecisionTaskStartedEvent(newDecision.ScheduleID, "request-from-RespondDecisionTaskCompleted", &types.PollForDecisionTaskRequest{
					TaskList: &types.TaskList{Name: common.StringPtr(newDecision.TaskList)},
					Identity: request.Identity,
				})
				if err != nil {
					return nil, err
				}
			}
		}

		// We apply the update to execution using optimistic concurrency.  If it fails due to a conflict then reload
		// the history and try the operation again.
		var updateErr error
		if continueAsNewBuilder != nil {
			continueAsNewExecutionInfo := continueAsNewBuilder.GetExecutionInfo()
			updateErr = wfContext.UpdateWorkflowExecutionWithNewAsActive(
				ctx,
				handler.shard.GetTimeSource().Now(),
				execution.NewContext(
					continueAsNewExecutionInfo.DomainID,
					types.WorkflowExecution{
						WorkflowID: common.StringPtr(continueAsNewExecutionInfo.WorkflowID),
						RunID:      common.StringPtr(continueAsNewExecutionInfo.RunID),
					},
					handler.shard,
					handler.shard.GetExecutionManager(),
					handler.logger,
				),
				continueAsNewBuilder,
			)
		} else {
			updateErr = wfContext.UpdateWorkflowExecutionAsActive(ctx, handler.shard.GetTimeSource().Now())
		}

		if updateErr != nil {
			if updateErr == execution.ErrConflict {
				handler.metricsClient.IncCounter(metrics.HistoryRespondDecisionTaskCompletedScope, metrics.ConcurrencyUpdateFailureCounter)
				continue Update_History_Loop
			}

			// if updateErr resulted in TransactionSizeLimitError then fail workflow
			switch updateErr.(type) {
			case *persistence.TransactionSizeLimitError:
				// must reload mutable state because the first call to updateWorkflowExecutionWithContext or continueAsNewWorkflowExecution
				// clears mutable state if error is returned
				msBuilder, err = wfContext.LoadWorkflowExecution(ctx)
				if err != nil {
					return nil, err
				}

				eventBatchFirstEventID := msBuilder.GetNextEventID()
				if err := execution.TerminateWorkflow(
					msBuilder,
					eventBatchFirstEventID,
					common.FailureReasonTransactionSizeExceedsLimit,
					[]byte(updateErr.Error()),
					identityHistoryService,
				); err != nil {
					return nil, err
				}
				if err := wfContext.UpdateWorkflowExecutionAsActive(
					ctx,
					handler.shard.GetTimeSource().Now(),
				); err != nil {
					return nil, err
				}
			}

			return nil, updateErr
		}

		handler.handleBufferedQueries(
			msBuilder,
			clientImpl,
			clientFeatureVersion,
			req.GetCompleteRequest().GetQueryResults(),
			createNewDecisionTask,
			domainEntry,
			decisionHeartbeating)

		if decisionHeartbeatTimeout {
			// at this point, update is successful, but we still return an error to client so that the worker will give up this workflow
			return nil, &types.EntityNotExistsError{
				Message: fmt.Sprintf("decision heartbeat timeout"),
			}
		}

		resp = &types.HistoryRespondDecisionTaskCompletedResponse{}
		activitiesToDispatchLocally := make(map[string]*types.ActivityLocalDispatchInfo)
		for _, dr := range decisionResults {
			if dr.activityDispatchInfo != nil {
				activitiesToDispatchLocally[*dr.activityDispatchInfo.ActivityID] = dr.activityDispatchInfo
			}
		}
		resp.ActivitiesToDispatchLocally = activitiesToDispatchLocally

		if request.GetReturnNewDecisionTask() && createNewDecisionTask {
			decision, _ := msBuilder.GetDecisionInfo(newDecisionTaskScheduledID)
			resp.StartedResponse, err = handler.createRecordDecisionTaskStartedResponse(domainID, msBuilder, decision, request.GetIdentity())
			if err != nil {
				return nil, err
			}
			// sticky is always enabled when worker request for new decision task from RespondDecisionTaskCompleted
			resp.StartedResponse.StickyExecutionEnabled = common.BoolPtr(true)
		}

		return resp, nil
	}

	return nil, ErrMaxAttemptsExceeded
}

func (handler *decisionHandlerImpl) createRecordDecisionTaskStartedResponse(
	domainID string,
	msBuilder execution.MutableState,
	decision *execution.DecisionInfo,
	identity string,
) (*types.RecordDecisionTaskStartedResponse, error) {

	response := &types.RecordDecisionTaskStartedResponse{}
	response.WorkflowType = msBuilder.GetWorkflowType()
	executionInfo := msBuilder.GetExecutionInfo()
	if executionInfo.LastProcessedEvent != common.EmptyEventID {
		response.PreviousStartedEventID = common.Int64Ptr(executionInfo.LastProcessedEvent)
	}

	// Starting decision could result in different scheduleID if decision was transient and new new events came in
	// before it was started.
	response.ScheduledEventID = common.Int64Ptr(decision.ScheduleID)
	response.StartedEventID = common.Int64Ptr(decision.StartedID)
	response.StickyExecutionEnabled = common.BoolPtr(msBuilder.IsStickyTaskListEnabled())
	response.NextEventID = common.Int64Ptr(msBuilder.GetNextEventID())
	response.Attempt = common.Int64Ptr(decision.Attempt)
	response.WorkflowExecutionTaskList = &types.TaskList{
		Name: &executionInfo.TaskList,
		Kind: types.TaskListKindNormal.Ptr(),
	}
	response.ScheduledTimestamp = common.Int64Ptr(decision.ScheduledTimestamp)
	response.StartedTimestamp = common.Int64Ptr(decision.StartedTimestamp)

	if decision.Attempt > 0 {
		// This decision is retried from mutable state
		// Also return schedule and started which are not written to history yet
		scheduledEvent, startedEvent := msBuilder.CreateTransientDecisionEvents(decision, identity)
		response.DecisionInfo = &types.TransientDecisionInfo{}
		response.DecisionInfo.ScheduledEvent = scheduledEvent
		response.DecisionInfo.StartedEvent = startedEvent
	}
	currentBranchToken, err := msBuilder.GetCurrentBranchToken()
	if err != nil {
		return nil, err
	}
	response.BranchToken = currentBranchToken

	qr := msBuilder.GetQueryRegistry()
	buffered := qr.GetBufferedIDs()
	queries := make(map[string]*types.WorkflowQuery)
	for _, id := range buffered {
		input, err := qr.GetQueryInput(id)
		if err != nil {
			continue
		}
		queries[id] = input
	}
	response.Queries = queries
	return response, nil
}

func (handler *decisionHandlerImpl) handleBufferedQueries(
	msBuilder execution.MutableState,
	clientImpl string,
	clientFeatureVersion string,
	queryResults map[string]*types.WorkflowQueryResult,
	createNewDecisionTask bool,
	domainEntry *cache.DomainCacheEntry,
	decisionHeartbeating bool,
) {
	queryRegistry := msBuilder.GetQueryRegistry()
	if !queryRegistry.HasBufferedQuery() {
		return
	}

	domainID := domainEntry.GetInfo().ID
	domain := domainEntry.GetInfo().Name
	workflowID := msBuilder.GetExecutionInfo().WorkflowID
	runID := msBuilder.GetExecutionInfo().RunID

	scope := handler.metricsClient.Scope(
		metrics.HistoryRespondDecisionTaskCompletedScope,
		metrics.DomainTag(domainEntry.GetInfo().Name),
		metrics.DecisionTypeTag("ConsistentQuery"))

	// Consistent query requires both server and client worker support. If a consistent query was requested (meaning there are
	// buffered queries) but worker does not support consistent query then all buffered queries should be failed.
	if versionErr := handler.versionChecker.SupportsConsistentQuery(clientImpl, clientFeatureVersion); versionErr != nil {
		scope.IncCounter(metrics.WorkerNotSupportsConsistentQueryCount)
		failedTerminationState := &query.TerminationState{
			TerminationType: query.TerminationTypeFailed,
			Failure:         &types.BadRequestError{Message: versionErr.Error()},
		}
		buffered := queryRegistry.GetBufferedIDs()
		handler.logger.Info(
			"failing query because worker does not support consistent query",
			tag.WorkflowDomainName(domain),
			tag.WorkflowID(workflowID),
			tag.WorkflowRunID(runID),
			tag.Error(versionErr))
		for _, id := range buffered {
			if err := queryRegistry.SetTerminationState(id, failedTerminationState); err != nil {
				handler.logger.Error(
					"failed to set query termination state to failed",
					tag.WorkflowDomainName(domain),
					tag.WorkflowID(workflowID),
					tag.WorkflowRunID(runID),
					tag.QueryID(id),
					tag.Error(err))
				scope.IncCounter(metrics.QueryRegistryInvalidStateCount)
			}
		}
		return
	}

	// if its a heartbeat decision it means local activities may still be running on the worker
	// which were started by an external event which happened before the query
	if decisionHeartbeating {
		return
	}

	sizeLimitError := handler.config.BlobSizeLimitError(domain)
	sizeLimitWarn := handler.config.BlobSizeLimitWarn(domain)

	// Complete or fail all queries we have results for
	for id, result := range queryResults {
		if err := common.CheckEventBlobSizeLimit(
			len(result.GetAnswer()),
			sizeLimitWarn,
			sizeLimitError,
			domainID,
			workflowID,
			runID,
			scope,
			handler.throttledLogger,
			tag.BlobSizeViolationOperation("ConsistentQuery"),
		); err != nil {
			handler.logger.Info("failing query because query result size is too large",
				tag.WorkflowDomainName(domain),
				tag.WorkflowID(workflowID),
				tag.WorkflowRunID(runID),
				tag.QueryID(id),
				tag.Error(err))
			failedTerminationState := &query.TerminationState{
				TerminationType: query.TerminationTypeFailed,
				Failure:         err,
			}
			if err := queryRegistry.SetTerminationState(id, failedTerminationState); err != nil {
				handler.logger.Error(
					"failed to set query termination state to failed",
					tag.WorkflowDomainName(domain),
					tag.WorkflowID(workflowID),
					tag.WorkflowRunID(runID),
					tag.QueryID(id),
					tag.Error(err))
				scope.IncCounter(metrics.QueryRegistryInvalidStateCount)
			}
		} else {
			completedTerminationState := &query.TerminationState{
				TerminationType: query.TerminationTypeCompleted,
				QueryResult:     result,
			}
			if err := queryRegistry.SetTerminationState(id, completedTerminationState); err != nil {
				handler.logger.Error(
					"failed to set query termination state to completed",
					tag.WorkflowDomainName(domain),
					tag.WorkflowID(workflowID),
					tag.WorkflowRunID(runID),
					tag.QueryID(id),
					tag.Error(err))
				scope.IncCounter(metrics.QueryRegistryInvalidStateCount)
			}
		}
	}

	// If no decision task was created then it means no buffered events came in during this decision task's handling.
	// This means all unanswered buffered queries can be dispatched directly through matching at this point.
	if !createNewDecisionTask {
		buffered := queryRegistry.GetBufferedIDs()
		for _, id := range buffered {
			unblockTerminationState := &query.TerminationState{
				TerminationType: query.TerminationTypeUnblocked,
			}
			if err := queryRegistry.SetTerminationState(id, unblockTerminationState); err != nil {
				handler.logger.Error(
					"failed to set query termination state to unblocked",
					tag.WorkflowDomainName(domain),
					tag.WorkflowID(workflowID),
					tag.WorkflowRunID(runID),
					tag.QueryID(id),
					tag.Error(err))
				scope.IncCounter(metrics.QueryRegistryInvalidStateCount)
			}
		}
	}
}
