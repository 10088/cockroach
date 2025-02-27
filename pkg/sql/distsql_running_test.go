// Copyright 2018 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package sql

import (
	"context"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/kv"
	"github.com/cockroachdb/cockroach/pkg/kv/kvclient/kvcoord"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/security/username"
	"github.com/cockroachdb/cockroach/pkg/sql/clusterunique"
	"github.com/cockroachdb/cockroach/pkg/sql/distsql"
	"github.com/cockroachdb/cockroach/pkg/sql/execinfra"
	"github.com/cockroachdb/cockroach/pkg/sql/execinfrapb"
	"github.com/cockroachdb/cockroach/pkg/sql/parser"
	"github.com/cockroachdb/cockroach/pkg/sql/rowenc"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/sessiondatapb"
	"github.com/cockroachdb/cockroach/pkg/testutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/pgtest"
	"github.com/cockroachdb/cockroach/pkg/testutils/serverutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/sqlutils"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/randutil"
	"github.com/cockroachdb/cockroach/pkg/util/tracing"
	"github.com/cockroachdb/cockroach/pkg/util/uuid"
	"github.com/cockroachdb/datadriven"
	"github.com/cockroachdb/errors"
	"github.com/stretchr/testify/require"
)

// Test that we don't attempt to create flows in an aborted transaction.
// Instead, a retryable error is created on the gateway. The point is to
// simulate a race where the heartbeat loop finds out that the txn is aborted
// just before a plan starts execution and check that we don't create flows in
// an aborted txn (which isn't allowed). Note that, once running, each flow can
// discover on its own that its txn is aborted - that's handled separately. But
// flows can't start in a txn that's already known to be aborted.
//
// We test this by manually aborting a txn and then attempting to execute a plan
// in it. We're careful to not use the transaction for anything but running the
// plan; planning will be performed outside of the transaction.
func TestDistSQLRunningInAbortedTxn(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	ctx := context.Background()
	s, sqlDB, db := serverutils.StartServer(t, base.TestServerArgs{})
	defer s.Stopper().Stop(ctx)

	if _, err := sqlDB.ExecContext(
		ctx, "create database test; create table test.t(a int)"); err != nil {
		t.Fatal(err)
	}
	key := roachpb.Key("a")

	// Plan a statement.
	execCfg := s.ExecutorConfig().(ExecutorConfig)
	internalPlanner, cleanup := NewInternalPlanner(
		"test",
		kv.NewTxn(ctx, db, s.NodeID()),
		username.RootUserName(),
		&MemoryMetrics{},
		&execCfg,
		sessiondatapb.SessionData{},
	)
	defer cleanup()
	p := internalPlanner.(*planner)
	query := "select * from test.t"
	stmt, err := parser.ParseOne(query)
	if err != nil {
		t.Fatal(err)
	}

	push := func(ctx context.Context, key roachpb.Key) error {
		// Conflicting transaction that pushes another transaction.
		conflictTxn := kv.NewTxn(ctx, db, 0 /* gatewayNodeID */)
		// We need to explicitly set a high priority for the push to happen.
		if err := conflictTxn.SetUserPriority(roachpb.MaxUserPriority); err != nil {
			return err
		}
		// Push through a Put, as opposed to a Get, so that the pushee gets aborted.
		if err := conflictTxn.Put(ctx, key, "pusher was here"); err != nil {
			return err
		}
		err = conflictTxn.Commit(ctx)
		require.NoError(t, err)
		t.Log(conflictTxn.Rollback(ctx))
		return err
	}

	// Make a db with a short heartbeat interval, so that the aborted txn finds
	// out quickly.
	ambient := s.AmbientCtx()
	tsf := kvcoord.NewTxnCoordSenderFactory(
		kvcoord.TxnCoordSenderFactoryConfig{
			AmbientCtx: ambient,
			// Short heartbeat interval.
			HeartbeatInterval: time.Millisecond,
			Settings:          s.ClusterSettings(),
			Clock:             s.Clock(),
			Stopper:           s.Stopper(),
		},
		s.DistSenderI().(*kvcoord.DistSender),
	)
	shortDB := kv.NewDB(ambient, tsf, s.Clock(), s.Stopper())

	iter := 0
	// We'll trace to make sure the test isn't fooling itself.
	tr := s.TracerI().(*tracing.Tracer)
	runningCtx, getRecAndFinish := tracing.ContextWithRecordingSpan(ctx, tr, "test")
	defer getRecAndFinish()
	err = shortDB.Txn(runningCtx, func(ctx context.Context, txn *kv.Txn) error {
		iter++
		if iter == 1 {
			// On the first iteration, abort the txn.

			if err := txn.Put(ctx, key, "val"); err != nil {
				t.Fatal(err)
			}

			if err := push(ctx, key); err != nil {
				t.Fatal(err)
			}

			// Now wait until the heartbeat loop notices that the transaction is aborted.
			testutils.SucceedsSoon(t, func() error {
				if txn.Sender().(*kvcoord.TxnCoordSender).IsTracking() {
					return fmt.Errorf("txn heartbeat loop running")
				}
				return nil
			})
		}

		// Create and run a DistSQL plan.
		rw := NewCallbackResultWriter(func(ctx context.Context, row tree.Datums) error {
			return nil
		})
		recv := MakeDistSQLReceiver(
			ctx,
			rw,
			stmt.AST.StatementReturnType(),
			execCfg.RangeDescriptorCache,
			txn,
			execCfg.Clock,
			p.ExtendedEvalContext().Tracing,
		)

		// We need to re-plan every time, since the plan is closed automatically
		// by PlanAndRun() below making it unusable across retries.
		p.stmt = makeStatement(stmt, clusterunique.ID{})
		if err := p.makeOptimizerPlan(ctx); err != nil {
			t.Fatal(err)
		}
		defer p.curPlan.close(ctx)

		evalCtx := p.ExtendedEvalContext()
		// We need distribute = true so that executing the plan involves marshaling
		// the root txn meta to leaf txns. Local flows can start in aborted txns
		// because they just use the root txn.
		planCtx := execCfg.DistSQLPlanner.NewPlanningCtx(ctx, evalCtx, p, nil,
			DistributionTypeSystemTenantOnly)
		planCtx.stmtType = recv.stmtType

		execCfg.DistSQLPlanner.PlanAndRun(
			ctx, evalCtx, planCtx, txn, p.curPlan.main, recv, nil, /* finishedSetupFn */
		)
		return rw.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	if iter != 2 {
		t.Fatalf("expected two iterations, but txn took %d to succeed", iter)
	}
	if tracing.FindMsgInRecording(getRecAndFinish(), clientRejectedMsg) == -1 {
		t.Fatalf("didn't find expected message in trace: %s", clientRejectedMsg)
	}
}

// Test that the DistSQLReceiver overwrites previous errors as "better" errors
// come along.
func TestDistSQLReceiverErrorRanking(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	// This test goes through the trouble of creating a server because it wants to
	// create a txn. It creates the txn because it wants to test an interaction
	// between the DistSQLReceiver and the TxnCoordSender: the DistSQLReceiver
	// will feed retriable errors to the TxnCoordSender which will change those
	// errors to TransactionRetryWithProtoRefreshError.
	ctx := context.Background()
	s, _, db := serverutils.StartServer(t, base.TestServerArgs{})
	defer s.Stopper().Stop(ctx)

	txn := kv.NewTxn(ctx, db, s.NodeID())

	rw := &errOnlyResultWriter{}
	recv := MakeDistSQLReceiver(
		ctx,
		rw,
		tree.Rows, /* StatementReturnType */
		nil,       /* rangeCache */
		txn,
		nil, /* clockUpdater */
		&SessionTracing{},
	)

	retryErr := roachpb.NewErrorWithTxn(
		roachpb.NewTransactionRetryError(
			roachpb.RETRY_SERIALIZABLE, "test err"),
		txn.TestingCloneTxn()).GoError()

	abortErr := roachpb.NewErrorWithTxn(
		roachpb.NewTransactionAbortedError(
			roachpb.ABORT_REASON_ABORTED_RECORD_FOUND),
		txn.TestingCloneTxn()).GoError()

	errs := []struct {
		err    error
		expErr string
	}{
		{
			// Initial error, retriable.
			err:    retryErr,
			expErr: "TransactionRetryWithProtoRefreshError: TransactionRetryError",
		},
		{
			// A non-retriable error overwrites a retriable one.
			err:    fmt.Errorf("err1"),
			expErr: "err1",
		},
		{
			// Another non-retriable error doesn't overwrite the previous one.
			err:    fmt.Errorf("err2"),
			expErr: "err1",
		},
		{
			// A TransactionAbortedError overwrites anything.
			err:    abortErr,
			expErr: "TransactionRetryWithProtoRefreshError: TransactionAbortedError",
		},
		{
			// A non-aborted retriable error does not overried the
			// TransactionAbortedError.
			err:    retryErr,
			expErr: "TransactionRetryWithProtoRefreshError: TransactionAbortedError",
		},
	}

	for i, tc := range errs {
		recv.Push(nil, /* row */
			&execinfrapb.ProducerMetadata{
				Err: tc.err,
			})
		if !testutils.IsError(rw.Err(), tc.expErr) {
			t.Fatalf("%d: expected %s, got %s", i, tc.expErr, rw.Err())
		}
	}
}

// TestDistSQLReceiverReportsContention verifies that the distsql receiver
// reports contention events via an observable metric if they occur. This test
// additionally verifies that the metric stays at zero if there is no
// contention.
func TestDistSQLReceiverReportsContention(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	ctx := context.Background()
	testutils.RunTrueAndFalse(t, "contention", func(t *testing.T, contention bool) {
		// TODO(yuzefovich): add an onContentionEventCb() to
		// DistSQLRunTestingKnobs and use it here to accumulate contention
		// events.
		s, db, _ := serverutils.StartServer(t, base.TestServerArgs{})
		defer s.Stopper().Stop(ctx)

		// Disable sampling so that only our query (below) gets a trace.
		// Otherwise, we're subject to flakes when internal queries experience contention.
		_, err := db.Exec("SET CLUSTER SETTING sql.txn_stats.sample_rate = 0")
		require.NoError(t, err)

		sqlutils.CreateTable(
			t, db, "test", "x INT PRIMARY KEY", 1, sqlutils.ToRowFn(sqlutils.RowIdxFn),
		)

		tableID := sqlutils.QueryTableID(t, db, sqlutils.TestDB, "public", "test")
		contentionEventSubstring := fmt.Sprintf("tableID=%d indexID=1", tableID)

		if contention {
			// Begin a contending transaction.
			conn, err := db.Conn(ctx)
			require.NoError(t, err)
			defer func() {
				require.NoError(t, conn.Close())
			}()
			_, err = conn.ExecContext(ctx, "BEGIN; UPDATE test.test SET x = 10 WHERE x = 1;")
			require.NoError(t, err)
		}

		metrics := s.DistSQLServer().(*distsql.ServerImpl).Metrics
		metrics.ContendedQueriesCount.Clear()
		contentionRegistry := s.ExecutorConfig().(ExecutorConfig).ContentionRegistry
		otherConn, err := db.Conn(ctx)
		require.NoError(t, err)
		defer func() {
			require.NoError(t, otherConn.Close())
		}()
		// TODO(yuzefovich): turning the tracing ON won't be necessary once
		// always-on tracing is enabled.
		_, err = otherConn.ExecContext(ctx, `SET TRACING=on;`)
		require.NoError(t, err)
		txn, err := otherConn.BeginTx(ctx, nil)
		require.NoError(t, err)
		_, err = txn.ExecContext(ctx, `
			SET TRANSACTION PRIORITY HIGH;
			UPDATE test.test SET x = 100 WHERE x = 1;
		`)

		require.NoError(t, err)
		if contention {
			// Soft check to protect against flakiness where an internal query
			// causes the contention metric to increment.
			require.GreaterOrEqual(t, metrics.ContendedQueriesCount.Count(), int64(1))
		} else {
			require.Zero(
				t,
				metrics.ContendedQueriesCount.Count(),
				"contention metric unexpectedly non-zero when no contention events are produced",
			)
		}

		require.Equal(t, contention, strings.Contains(contentionRegistry.String(), contentionEventSubstring))
		err = txn.Commit()
		require.NoError(t, err)
		_, err = otherConn.ExecContext(ctx, `SET TRACING=off;`)
		require.NoError(t, err)
	})

}

// TestDistSQLReceiverDrainsOnError is a simple unit test that asserts that the
// DistSQLReceiver transitions to execinfra.DrainRequested status if an error is
// pushed into it.
func TestDistSQLReceiverDrainsOnError(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	recv := MakeDistSQLReceiver(
		context.Background(),
		&errOnlyResultWriter{},
		tree.Rows,
		nil, /* rangeCache */
		nil, /* txn */
		nil, /* clockUpdater */
		&SessionTracing{},
	)
	status := recv.Push(nil /* row */, &execinfrapb.ProducerMetadata{Err: errors.New("some error")})
	require.Equal(t, execinfra.DrainRequested, status)
}

// TestDistSQLReceiverDrainsMeta verifies that the DistSQLReceiver drains the
// execution flow in order to retrieve the required metadata. In particular, it
// sets up a 3 node cluster which is then accessed via PGWire protocol in order
// to take advantage of the LIMIT feature of portals (pausing the execution once
// the desired number of rows have been returned to the client). The crux of the
// test is, once the portal is closed and the execution flow is shutdown, making
// sure that the receiver collects LeafTxnFinalState metadata from each of the
// nodes which is required for correctness.
func TestDistSQLReceiverDrainsMeta(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	var accumulatedMeta []execinfrapb.ProducerMetadata
	// Set up a 3 node cluster and inject a callback to accumulate all metadata
	// for the test query.
	const numNodes = 3
	const testQuery = "SELECT * FROM foo"
	ctx := context.Background()
	tc := serverutils.StartNewTestCluster(t, numNodes, base.TestClusterArgs{
		ReplicationMode: base.ReplicationManual,
		ServerArgs: base.TestServerArgs{
			UseDatabase: "test",
			Knobs: base.TestingKnobs{
				SQLExecutor: &ExecutorTestingKnobs{
					DistSQLReceiverPushCallbackFactory: func(query string) func(rowenc.EncDatumRow, *execinfrapb.ProducerMetadata) {
						if query != testQuery {
							return nil
						}
						return func(row rowenc.EncDatumRow, meta *execinfrapb.ProducerMetadata) {
							if meta != nil {
								accumulatedMeta = append(accumulatedMeta, *meta)
							}
						}
					},
				},
			},
			Insecure: true,
		}})
	defer tc.Stopper().Stop(ctx)

	// Create a table with 30 rows, split them into 3 ranges with each node
	// having one.
	db := tc.ServerConn(0 /* idx */)
	sqlDB := sqlutils.MakeSQLRunner(db)
	sqlutils.CreateTable(
		t, db, "foo",
		"k INT PRIMARY KEY, v INT",
		30,
		sqlutils.ToRowFn(sqlutils.RowIdxFn, sqlutils.RowModuloFn(2)),
	)
	sqlDB.Exec(t, "ALTER TABLE test.foo SPLIT AT VALUES (10), (20)")
	sqlDB.Exec(
		t,
		fmt.Sprintf("ALTER TABLE test.foo EXPERIMENTAL_RELOCATE VALUES (ARRAY[%d], 0), (ARRAY[%d], 10), (ARRAY[%d], 20)",
			tc.Server(0).GetFirstStoreID(),
			tc.Server(1).GetFirstStoreID(),
			tc.Server(2).GetFirstStoreID(),
		),
	)

	// Connect to the cluster via the PGWire client.
	p, err := pgtest.NewPGTest(ctx, tc.Server(0).ServingSQLAddr(), username.RootUser)
	require.NoError(t, err)

	// Execute the test query asking for at most 25 rows.
	require.NoError(t, p.SendOneLine(`Query {"String": "USE test"}`))
	require.NoError(t, p.SendOneLine(fmt.Sprintf(`Parse {"Query": "%s"}`, testQuery)))
	require.NoError(t, p.SendOneLine(`Bind`))
	require.NoError(t, p.SendOneLine(`Execute {"MaxRows": 25}`))
	require.NoError(t, p.SendOneLine(`Sync`))

	// Retrieve all of the results. We need to receive until two 'ReadyForQuery'
	// messages are returned (the first one for "USE test" query and the second
	// one is for the limited portal execution).
	until := pgtest.ParseMessages("ReadyForQuery\nReadyForQuery")
	msgs, err := p.Until(false /* keepErrMsg */, until...)
	require.NoError(t, err)
	received := pgtest.MsgsToJSONWithIgnore(msgs, &datadriven.TestData{})

	// Confirm that we did retrieve 25 rows as well as 3 metadata objects.
	require.Equal(t, 25, strings.Count(received, `"Type":"DataRow"`))
	numLeafTxnFinalMeta := 0
	for _, meta := range accumulatedMeta {
		if meta.LeafTxnFinalState != nil {
			numLeafTxnFinalMeta++
		}
	}
	require.Equal(t, numNodes, numLeafTxnFinalMeta)
}

// TestCancelFlowsCoordinator performs sanity-checking of cancelFlowsCoordinator
// and that it can be safely used concurrently.
func TestCancelFlowsCoordinator(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	var c cancelFlowsCoordinator

	globalRng, _ := randutil.NewTestRand()
	numNodes := globalRng.Intn(16) + 2
	gatewaySQLInstanceID := base.SQLInstanceID(1)

	assertInvariants := func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		// Check that the coordinator hasn't created duplicate entries for some
		// nodes.
		require.GreaterOrEqual(t, numNodes-1, c.mu.deadFlowsByNode.Len())
		seen := make(map[base.SQLInstanceID]struct{})
		for i := 0; i < c.mu.deadFlowsByNode.Len(); i++ {
			deadFlows := c.mu.deadFlowsByNode.Get(i)
			require.NotEqual(t, gatewaySQLInstanceID, deadFlows.sqlInstanceID)
			_, ok := seen[deadFlows.sqlInstanceID]
			require.False(t, ok)
			seen[deadFlows.sqlInstanceID] = struct{}{}
		}
	}

	// makeFlowsToCancel returns a fake flows map where each node in the cluster
	// has 67% probability of participating in the plan.
	makeFlowsToCancel := func(rng *rand.Rand) map[base.SQLInstanceID]*execinfrapb.FlowSpec {
		res := make(map[base.SQLInstanceID]*execinfrapb.FlowSpec)
		flowID := execinfrapb.FlowID{UUID: uuid.FastMakeV4()}
		for id := 1; id <= numNodes; id++ {
			if rng.Float64() < 0.33 {
				// This node wasn't a part of the current plan.
				continue
			}
			res[base.SQLInstanceID(id)] = &execinfrapb.FlowSpec{
				FlowID:  flowID,
				Gateway: gatewaySQLInstanceID,
			}
		}
		return res
	}

	var wg sync.WaitGroup
	maxSleepTime := 100 * time.Millisecond

	// Spin up some goroutines that simulate query runners, with each hitting an
	// error and deciding to cancel all scheduled dead flows.
	numQueryRunners := globalRng.Intn(8) + 1
	numRunsPerRunner := globalRng.Intn(10) + 1
	wg.Add(numQueryRunners)
	for i := 0; i < numQueryRunners; i++ {
		go func() {
			defer wg.Done()
			rng, _ := randutil.NewTestRand()
			for i := 0; i < numRunsPerRunner; i++ {
				c.addFlowsToCancel(makeFlowsToCancel(rng))
				time.Sleep(time.Duration(rng.Int63n(int64(maxSleepTime))))
			}
		}()
	}

	// Have a single goroutine that checks the internal state of the coordinator
	// and retrieves the next request to cancel some flows (in order to simulate
	// the canceling worker).
	wg.Add(1)
	go func() {
		defer wg.Done()
		rng, _ := randutil.NewTestRand()
		done := time.After(2 * time.Second)
		for {
			select {
			case <-done:
				return
			default:
				assertInvariants()
				time.Sleep(time.Duration(rng.Int63n(int64(maxSleepTime))))
				// We're not interested in the result of this call.
				_, _ = c.getFlowsToCancel()
			}
		}
	}()

	wg.Wait()
}

// TestDistSQLRunnerCoordinator verifies that the runnerCoordinator correctly
// reacts to the changes of the corresponding setting.
func TestDistSQLRunnerCoordinator(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	ctx := context.Background()
	s, db, _ := serverutils.StartServer(t, base.TestServerArgs{})
	defer s.Stopper().Stop(ctx)

	runner := &s.ExecutorConfig().(ExecutorConfig).DistSQLPlanner.runnerCoordinator
	sqlDB := sqlutils.MakeSQLRunner(db)

	checkNumRunners := func(newNumRunners int64) {
		sqlDB.Exec(t, fmt.Sprintf("SET CLUSTER SETTING sql.distsql.num_runners = %d", newNumRunners))
		testutils.SucceedsSoon(t, func() error {
			numWorkers := atomic.LoadInt64(&runner.atomics.numWorkers)
			if numWorkers != newNumRunners {
				return errors.Newf("%d workers are up, want %d", numWorkers, newNumRunners)
			}
			return nil
		})
	}

	// Lower the setting to 0 and make sure that all runners exit.
	checkNumRunners(0)

	// Now bump it up to 100.
	checkNumRunners(100)
}

// TestSetupFlowRPCError verifies that the distributed query plan errors out and
// cleans up all flows if the SetupFlow RPC fails for one of the remote nodes.
// It also checks that the expected error is returned.
func TestSetupFlowRPCError(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	// Start a 3 node cluster where we can inject an error for SetupFlow RPC on
	// the server side for the queries in question.
	const numNodes = 3
	ctx := context.Background()
	getError := func(nodeID base.SQLInstanceID) error {
		return errors.Newf("injected error on n%d", nodeID)
	}
	// We use different queries to simplify handling the node ID on which the
	// error should be injected (i.e. we avoid the need for synchronization in
	// the test). In particular, the difficulty comes from the fact that some of
	// the SetupFlow RPCs might not be issued at all while others are served
	// after the corresponding flow on the gateway has exited.
	queries := []string{
		"SELECT k FROM test.foo",
		"SELECT v FROM test.foo",
		"SELECT * FROM test.foo",
	}
	stmtToNodeIDForError := map[string]base.SQLInstanceID{
		queries[0]: 2, // error on n2
		queries[1]: 3, // error on n3
		queries[2]: 0, // no error
	}
	tc := serverutils.StartNewTestCluster(t, numNodes, base.TestClusterArgs{
		ReplicationMode: base.ReplicationManual,
		ServerArgs: base.TestServerArgs{
			Knobs: base.TestingKnobs{
				DistSQL: &execinfra.TestingKnobs{
					SetupFlowCb: func(_ context.Context, nodeID base.SQLInstanceID, req *execinfrapb.SetupFlowRequest) error {
						nodeIDForError, ok := stmtToNodeIDForError[req.StatementSQL]
						if !ok || nodeIDForError != nodeID {
							return nil
						}
						return getError(nodeID)
					},
				},
			},
		},
	})
	defer tc.Stopper().Stop(ctx)

	// Create a table with 30 rows, split them into 3 ranges with each node
	// having one.
	db := tc.ServerConn(0)
	sqlDB := sqlutils.MakeSQLRunner(db)
	sqlutils.CreateTable(
		t, db, "foo",
		"k INT PRIMARY KEY, v INT",
		30,
		sqlutils.ToRowFn(sqlutils.RowIdxFn, sqlutils.RowModuloFn(2)),
	)
	sqlDB.Exec(t, "ALTER TABLE test.foo SPLIT AT VALUES (10), (20)")
	sqlDB.Exec(
		t,
		fmt.Sprintf("ALTER TABLE test.foo EXPERIMENTAL_RELOCATE VALUES (ARRAY[%d], 0), (ARRAY[%d], 10), (ARRAY[%d], 20)",
			tc.Server(0).GetFirstStoreID(),
			tc.Server(1).GetFirstStoreID(),
			tc.Server(2).GetFirstStoreID(),
		),
	)

	// assertNoRemoteFlows verifies that the remote flows exit "soon".
	//
	// Note that in practice this happens very quickly, but in an edge case it
	// could take 10s (sql.distsql.flow_stream_timeout). That edge case occurs
	// when the server-side goroutine of the SetupFlow RPC is scheduled after
	// - the gateway flow exits with an error
	// - the CancelDeadFlows RPC for the remote flow in question completes.
	// With such setup the FlowStream RPC of the outbox will time out after 10s.
	assertNoRemoteFlows := func() {
		testutils.SucceedsSoon(t, func() error {
			for i, remoteNode := range []*distsql.ServerImpl{
				tc.Server(1).DistSQLServer().(*distsql.ServerImpl),
				tc.Server(2).DistSQLServer().(*distsql.ServerImpl),
			} {
				if n := remoteNode.NumRemoteRunningFlows(); n != 0 {
					return errors.Newf("%d remote flows still running on n%d", n, i+2)
				}
			}
			return nil
		})
	}

	// Run query twice while injecting an error on the remote nodes.
	for i := 0; i < 2; i++ {
		query := queries[i]
		nodeID := stmtToNodeIDForError[query]
		t.Logf("running %q with error being injected on n%d", query, nodeID)
		_, err := db.ExecContext(ctx, query)
		require.True(t, strings.Contains(err.Error(), getError(nodeID).Error()))
		assertNoRemoteFlows()
	}

	// Sanity check that the query doesn't error out without error injection.
	t.Logf("running %q with no error injection", queries[2])
	_, err := db.ExecContext(ctx, queries[2])
	require.NoError(t, err)
	assertNoRemoteFlows()
}
