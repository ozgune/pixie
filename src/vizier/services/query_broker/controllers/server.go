package controllers

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/gogo/protobuf/proto"
	"github.com/nats-io/nats.go"
	uuid "github.com/satori/go.uuid"
	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"pixielabs.ai/pixielabs/src/vizier/services/metadata/metadatapb"
	"pixielabs.ai/pixielabs/src/vizier/services/query_broker/tracker"

	"pixielabs.ai/pixielabs/src/carnot/planner/distributedpb"
	"pixielabs.ai/pixielabs/src/carnot/queryresultspb"
	"pixielabs.ai/pixielabs/src/carnotpb"
	vizierpb "pixielabs.ai/pixielabs/src/vizier/vizierpb"

	logicalplanner "pixielabs.ai/pixielabs/src/carnot/planner"
	"pixielabs.ai/pixielabs/src/carnot/planner/plannerpb"
	"pixielabs.ai/pixielabs/src/carnot/planpb"
	"pixielabs.ai/pixielabs/src/carnot/udfspb"
	statuspb "pixielabs.ai/pixielabs/src/common/base/proto"
	"pixielabs.ai/pixielabs/src/utils"
	funcs "pixielabs.ai/pixielabs/src/vizier/funcs/export"
	"pixielabs.ai/pixielabs/src/vizier/services/query_broker/querybrokerenv"
	"pixielabs.ai/pixielabs/src/vizier/services/query_broker/querybrokerpb"
)

const healthCheckInterval = 5 * time.Second

type contextKey string

const (
	execStartKey               = contextKey("execStart")
	compileCompleteKey         = contextKey("compileDone")
	transferResultChunkTimeout = 5 * time.Second
)

// Planner describes the interface for any planner.
type Planner interface {
	Plan(planState *distributedpb.LogicalPlannerState, req *plannerpb.QueryRequest) (*distributedpb.LogicalPlannerResult, error)
	CompileMutations(planState *distributedpb.LogicalPlannerState, request *plannerpb.CompileMutationsRequest) (*plannerpb.CompileMutationsResponse, error)
	Free()
}

// Executor is the interface for a query executor.
type Executor interface {
	ExecuteQuery(planMap map[uuid.UUID]*planpb.Plan, analyze bool) error
	WaitForCompletion() (*queryresultspb.QueryResult, error)
	AddResult(res *querybrokerpb.AgentQueryResultRequest)
	GetQueryID() uuid.UUID
	AddStreamedResult(res *carnotpb.TransferResultChunkRequest) error
}

// AgentsTracker is the interface for the background agent information tracker.
type AgentsTracker interface {
	GetAgentInfo() tracker.AgentsInfo
}

// Server defines an gRPC server type.
type Server struct {
	env           querybrokerenv.QueryBrokerEnv
	agentsTracker AgentsTracker
	natsConn      *nats.Conn
	// Mutex is used for managing query executor instances.
	mux sync.Mutex

	hcMux    sync.Mutex
	hcStatus error
	hcTime   time.Time

	mdtp            metadatapb.MetadataTracepointServiceClient
	resultForwarder QueryResultForwarder

	planner Planner
}

// NewServer creates GRPC handlers.
func NewServer(env querybrokerenv.QueryBrokerEnv, agentsTracker AgentsTracker,
	mds metadatapb.MetadataTracepointServiceClient,
	natsConn *nats.Conn) (*Server, error) {

	var udfInfo udfspb.UDFInfo
	if err := loadUDFInfo(&udfInfo); err != nil {
		return nil, err
	}

	return NewServerWithForwarderAndPlanner(env, agentsTracker, NewQueryResultForwarder(), mds,
		natsConn, logicalplanner.New(&udfInfo))
}

// NewServerWithForwarderAndPlanner is NewServer with a QueryResultForwarder and a planner generating func.
func NewServerWithForwarderAndPlanner(env querybrokerenv.QueryBrokerEnv,
	agentsTracker AgentsTracker,
	resultForwarder QueryResultForwarder,
	mds metadatapb.MetadataTracepointServiceClient,
	natsConn *nats.Conn,
	planner Planner) (*Server, error) {

	s := &Server{
		env:             env,
		agentsTracker:   agentsTracker,
		resultForwarder: resultForwarder,
		natsConn:        natsConn,
		mdtp:            mds,
		planner:         planner,
	}
	return s, nil
}

// Close frees the planner memory in the server.
func (s *Server) Close() {
	s.planner.Free()
}

func failedStatusQueryResponse(queryID uuid.UUID, status *statuspb.Status) *querybrokerpb.VizierQueryResponse {
	queryIDPB := utils.ProtoFromUUID(&queryID)
	queryResponse := &querybrokerpb.VizierQueryResponse{
		QueryID: queryIDPB,
		Status:  status,
	}
	return queryResponse
}

// runQuery executes a query and streams the results to the client.
// returns a bool for whether the query timed out and an error.
func (s *Server) runQuery(ctx context.Context, req *plannerpb.QueryRequest, queryID uuid.UUID,
	planOpts *planpb.PlanOptions, distributedState *distributedpb.DistributedState,
	resultStream chan *vizierpb.ExecuteScriptResponse, doneCh chan bool) error {

	log.WithField("query_id", queryID).Infof("Running script")
	start := time.Now()
	defer func(t time.Time) {
		duration := time.Now().Sub(t)
		log.WithField("query_id", queryID).WithField("duration", duration).Info("Executed query")
	}(start)

	defer func() {
		close(doneCh)
	}()

	ctx = context.WithValue(ctx, execStartKey, time.Now())

	info := s.agentsTracker.GetAgentInfo()
	if info == nil {
		return status.Error(codes.Unavailable, "not ready yet")
	}
	plannerState := &distributedpb.LogicalPlannerState{
		DistributedState:    distributedState,
		PlanOptions:         planOpts,
		ResultAddress:       s.env.Address(),
		ResultSSLTargetName: s.env.SSLTargetName(),
	}

	// Compile the query plan.
	plannerResultPB, err := s.planner.Plan(plannerState, req)

	if err != nil {
		// send the compilation error and return nil.
		return err
	}

	compilationTimeNs := time.Now().Sub(start).Nanoseconds()

	// When the status is not OK, this means it's a compilation error on the query passed in.
	if plannerResultPB.Status.ErrCode != statuspb.OK {
		resultStream <- StatusToVizierResponse(queryID, plannerResultPB.Status)
		return nil
	}

	// Plan describes the mapping of agents to the plan that should execute on them.
	plan := plannerResultPB.Plan
	planMap := make(map[uuid.UUID]*planpb.Plan)

	for carnotID, agentPlan := range plan.QbAddressToPlan {
		u, err := uuid.FromString(carnotID)
		if err != nil {
			log.WithError(err).Fatalf("Couldn't parse uuid from agent id \"%s\"", carnotID)
			return err
		}
		planMap[u] = agentPlan
	}

	queryPlanTableID := uuid.NewV4().String()
	tableNameToIDMap := make(map[string]string)

	for _, plan := range planMap {
		for _, fragment := range plan.Nodes {
			for _, node := range fragment.Nodes {
				if node.Op.OpType == planpb.GRPC_SINK_OPERATOR {
					if output := node.Op.GetGRPCSinkOp().GetOutputTable(); output != nil {
						tableNameToIDMap[output.TableName] = uuid.NewV4().String()
					}
				}
			}
		}
	}

	tableRelationResponses, err := TableRelationResponses(queryID, tableNameToIDMap, planMap)
	for _, resp := range tableRelationResponses {
		resultStream <- resp
	}

	err = s.resultForwarder.RegisterQuery(queryID, tableNameToIDMap)
	if err != nil {
		return err
	}
	err = LaunchQuery(queryID, s.natsConn, planMap, planOpts.Analyze)
	if err != nil {
		s.resultForwarder.DeleteQuery(queryID)
		return err
	}

	// Send over the query plan responses, if applicable.
	var queryPlanOpts *QueryPlanOpts
	if planOpts.Explain {
		resultStream <- QueryPlanRelationResponse(queryID, queryPlanTableID)
		queryPlanOpts = &QueryPlanOpts{
			TableID: queryPlanTableID,
			Plan:    plan,
			PlanMap: planMap,
		}
	}

	return s.resultForwarder.StreamResults(ctx, queryID, resultStream,
		compilationTimeNs, queryPlanOpts)
}

func loadUDFInfo(udfInfoPb *udfspb.UDFInfo) error {
	b, err := funcs.Asset("src/vizier/funcs/data/udf.pb")
	if err != nil {
		return err
	}
	proto.Unmarshal(b, udfInfoPb)
	return nil
}

// CheckHealth runs the health check and returns an error if it didn't pass.
func (s *Server) CheckHealth(ctx context.Context) error {
	checkVersionScript := `import px; px.display(px.Version())`
	req := &plannerpb.QueryRequest{
		QueryStr: checkVersionScript,
	}

	flags, err := ParseQueryFlags(req.QueryStr)
	if err != nil {
		return fmt.Errorf("error parsing query flags: %v", err)
	}
	queryID := uuid.NewV4()
	planOpts := flags.GetPlanOptions()

	resultStream := make(chan *vizierpb.ExecuteScriptResponse)
	doneCh := make(chan bool)

	var wg sync.WaitGroup
	receivedRowBatches := 0
	receivedRows := int64(0)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	wg.Add(1)

	go func() {
		defer func() {
			close(resultStream)
		}()
		for {
			select {
			case <-time.After(healthCheckInterval):
				wg.Done()
				cancel()
				return
			case <-doneCh:
				wg.Done()
				return
			case result := <-resultStream:
				if data := result.GetData(); data != nil {
					if rb := data.GetBatch(); rb != nil {
						receivedRowBatches++
						receivedRows += rb.NumRows
					}
				}
			}
		}
	}()

	distributedState := s.agentsTracker.GetAgentInfo().DistributedState()
	err = s.runQuery(ctx, req, queryID, planOpts, &distributedState, resultStream, doneCh)
	if err != nil {
		return fmt.Errorf("error running healthcheck query ID %s: %v", queryID.String(), err)
	}

	wg.Wait()

	if receivedRowBatches == 0 || receivedRows == int64(0) {
		return fmt.Errorf("results not returned on health check for query ID %s", queryID.String())
	}

	if receivedRowBatches > 1 || receivedRows > int64(1) {
		// We expect only one row to be received from this query.
		return fmt.Errorf("bad results on healthcheck for query ID %s", queryID.String())
	}

	return nil
}

func (s *Server) checkHealthCached(ctx context.Context) error {
	currentTime := time.Now()
	s.hcMux.Lock()
	defer s.hcMux.Unlock()
	if currentTime.Sub(s.hcTime) < healthCheckInterval {
		return s.hcStatus
	}
	status := s.CheckHealth(ctx)
	if status != nil {
		// If the request failed don't cache the results.
		return status
	}
	s.hcTime = currentTime
	s.hcStatus = status
	return s.hcStatus
}

// HealthCheck continually responds with the current health of Vizier.
func (s *Server) HealthCheck(req *vizierpb.HealthCheckRequest, srv vizierpb.VizierService_HealthCheckServer) error {
	// For now, just report itself as healthy.
	for c := time.Tick(healthCheckInterval); ; {
		hcResult := s.checkHealthCached(srv.Context())
		// Pass.
		code := int32(codes.OK)
		if hcResult != nil {
			log.Infof("Received unhealthy heath check result: %s", hcResult.Error())
			code = int32(codes.Unavailable)
		}
		err := srv.Send(&vizierpb.HealthCheckResponse{
			Status: &vizierpb.Status{
				Code: code,
			},
		})
		if err != nil {
			log.WithError(err).Error("Error sending on stream, ending health check")
			return err
		}
		select {
		case <-srv.Context().Done():
			return nil
		case <-c:
			continue
		}
	}
}

// ExecuteScript executes the script and sends results through the gRPC stream.
func (s *Server) ExecuteScript(req *vizierpb.ExecuteScriptRequest, srv vizierpb.VizierService_ExecuteScriptServer) error {
	ctx := context.WithValue(srv.Context(), execStartKey, time.Now())
	// TODO(philkuz) we should move the query id into the api so we can track how queries propagate through the system.
	queryID := uuid.NewV4()

	flags, err := ParseQueryFlags(req.QueryStr)
	if err != nil {
		return srv.Send(ErrToVizierResponse(queryID, err))
	}

	planOpts := flags.GetPlanOptions()

	distributedState := s.agentsTracker.GetAgentInfo().DistributedState()

	if req.Mutation {
		mutationExec := NewMutationExecutor(s.planner, s.mdtp, &distributedState)

		status, err := mutationExec.Execute(ctx, req, planOpts)
		if err != nil {
			return srv.Send(ErrToVizierResponse(queryID, err))
		}
		if status != nil {
			return srv.Send(StatusToVizierResponse(queryID, status))
		}
		mutationInfo, err := mutationExec.MutationInfo(ctx)
		if err != nil {
			return srv.Send(ErrToVizierResponse(queryID, err))
		}
		err = srv.Send(&vizierpb.ExecuteScriptResponse{
			QueryID:      queryID.String(),
			MutationInfo: mutationInfo,
		})

		if mutationInfo.Status.Code != int32(codes.OK) || err != nil {
			return srv.Send(ErrToVizierResponse(queryID, err))
		}
	}

	// Convert request to a format expected by the planner.
	convertedReq, err := VizierQueryRequestToPlannerQueryRequest(req)
	if err != nil {
		return err
	}

	resultStream := make(chan *vizierpb.ExecuteScriptResponse)
	doneCh := make(chan bool)

	var wg sync.WaitGroup
	wg.Add(1)

	var sendErr error
	go func() {
		for {
			select {
			case <-doneCh:
				close(resultStream)
				wg.Done()
				return
			case result := <-resultStream:
				err = srv.Send(result)
				if err != nil {
					sendErr = err
				}
			}
		}
	}()

	log.Infof("Launching query: %s", queryID)
	err = s.runQuery(ctx, convertedReq, queryID, planOpts, &distributedState, resultStream, doneCh)
	wg.Wait()

	if err != nil {
		return err
	}
	if sendErr != nil {
		return err
	}
	return nil
}

// TransferResultChunk implements the API that allows the query broker receive streamed results
// from Carnot instances.
func (s *Server) TransferResultChunk(srv carnotpb.ResultSinkService_TransferResultChunkServer) error {
	var queryID uuid.UUID
	// will be set if this particular stream is sending the results for a table.
	// not set if it is sending the results for exec stats.
	var tableName string
	// if tableName is set, tracks whether this particular stream has sent EOS.
	// used to cancel the query if the connection is closed by the agent before EOS is sent.
	sentEos := false

	sendAndClose := func(success bool, message string) error {
		err := srv.SendAndClose(&carnotpb.TransferResultChunkResponse{
			Success: success,
			Message: message,
		})
		if !success {
			if queryID == uuid.Nil {
				log.Errorf("TransferResultChunk encountered an error for unknown query ID: %s", message)
			} else {
				// Stop the client stream, if it still exists in the result forwarder.
				// It may have already been cancelled before this point.
				log.Errorf("TransferResultChunk cancelling client stream for query %s", queryID.String())
				s.resultForwarder.OptionallyCancelClientStream(queryID)
			}
		}
		return err
	}

	handleAgentStreamClosed := func() error {
		if tableName != "" && !sentEos {
			// Send an error and cancel the query if the stream is closed unexpectedly.
			return sendAndClose( /*success*/ false, fmt.Sprintf(
				"agent stream was unxpectedly closed for table %s of query %s before the results completed",
				tableName, queryID.String(),
			))
		}
		return sendAndClose( /*success*/ true, "")
	}

	for {
		select {
		case <-srv.Context().Done():
			return handleAgentStreamClosed()
		default:
			msg, err := srv.Recv()
			// Stream closed from client side.
			if err != nil && err == io.EOF {
				return handleAgentStreamClosed()
			}
			if err != nil {
				return sendAndClose( /*success*/ false, fmt.Sprintf(
					"Error reading TransferResultChunk stream: %s", err.Error()))
			}

			qid, err := utils.UUIDFromProto(msg.QueryID)
			if err != nil {
				return sendAndClose( /*success*/ false, err.Error())
			}

			if queryID == uuid.Nil {
				queryID = qid
			}
			if queryID != qid {
				return sendAndClose( /*success*/ false, fmt.Sprintf(
					"Received results from multiple queries in the same TransferResultChunk stream: %s and %s",
					queryID, qid,
				))
			}

			err = s.resultForwarder.ForwardQueryResult(msg)
			// If the result wasn't forwarded, the client stream may have be cancelled.
			// This should not cause TransferResultChunk to return an error, we just include in the response
			// that the latest result chunk was not forwarded.
			if err != nil {
				log.WithError(err).Infof("Could not forward result message for query %s", queryID)
				return sendAndClose( /*success*/ false, err.Error())
			}

			// Keep track of which table this stream is sending results for, and if it has sent EOS yet.
			if queryResult := msg.GetQueryResult(); queryResult != nil {
				if tableName == "" {
					tableName = queryResult.GetTableName()
				}
				if tableName != queryResult.GetTableName() {
					return sendAndClose( /*success*/ false, fmt.Sprintf(
						"Received results from multiple tables for query %s in the same TransferResultChunk stream"+
							": %s and %s", queryID.String(), tableName, queryResult.GetTableName(),
					))
				}
				if batch := queryResult.GetRowBatch(); batch != nil {
					if batch.GetEos() {
						sentEos = true
					}
				}
			}
		}
	}
}
