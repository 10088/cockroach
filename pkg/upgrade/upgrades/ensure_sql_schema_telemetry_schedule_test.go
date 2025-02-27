// Copyright 2022 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package upgrades_test

import (
	"context"
	"fmt"
	"regexp"
	"testing"
	"time"

	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/clusterversion"
	"github.com/cockroachdb/cockroach/pkg/jobs"
	"github.com/cockroachdb/cockroach/pkg/jobs/jobstest"
	"github.com/cockroachdb/cockroach/pkg/kv"
	"github.com/cockroachdb/cockroach/pkg/server"
	"github.com/cockroachdb/cockroach/pkg/sql"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/schematelemetry/schematelemetrycontroller"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/builtins/builtinconstants"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/testutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/skip"
	"github.com/cockroachdb/cockroach/pkg/testutils/sqlutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/testcluster"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/syncutil"
	"github.com/cockroachdb/cockroach/pkg/util/timeutil"
	"github.com/cockroachdb/errors"
)

func TestSchemaTelemetrySchedule(t *testing.T) {
	defer leaktest.AfterTest(t)()

	skip.WithIssue(t, 95530, "bump minBinary to 22.2. Skip 22.2 mixed-version tests for future cleanup")

	// We want to ensure that the migration will succeed when run again.
	// To ensure that it will, we inject a failure when trying to mark
	// the upgrade as complete when forceRetry is true.
	testutils.RunTrueAndFalse(t, "force-retry", func(t *testing.T, forceRetry bool) {
		defer log.Scope(t).Close(t)

		ctx := context.Background()
		var args base.TestServerArgs
		var injectedFailure syncutil.AtomicBool
		// The statement which writes the completion of the migration will
		// match the below regexp.
		completeRegexp := regexp.MustCompile(`INSERT\s+INTO\s+system.migrations`)
		jobKnobs := jobs.NewTestingKnobsWithShortIntervals()
		jobKnobs.JobSchedulerEnv = jobstest.NewJobSchedulerTestEnv(
			jobstest.UseSystemTables,
			timeutil.Now(),
			tree.ScheduledSchemaTelemetryExecutor,
		)
		args.Knobs.JobsTestingKnobs = jobKnobs
		args.Knobs.SQLExecutor = &sql.ExecutorTestingKnobs{
			BeforePrepare: func(ctx context.Context, stmt string, txn *kv.Txn) error {
				if forceRetry && !injectedFailure.Get() && completeRegexp.MatchString(stmt) {
					injectedFailure.Set(true)
					return errors.New("boom")
				}
				return nil
			},
		}
		aostDuration := time.Nanosecond
		args.Knobs.SchemaTelemetry = &sql.SchemaTelemetryTestingKnobs{
			AOSTDuration: &aostDuration,
		}
		args.Knobs.Server = &server.TestingKnobs{
			DisableAutomaticVersionUpgrade: make(chan struct{}),
			BinaryVersionOverride:          clusterversion.ByKey(clusterversion.TODODelete_V22_2SQLSchemaTelemetryScheduledJobs - 1),
		}
		tc := testcluster.StartTestCluster(t, 1, base.TestClusterArgs{ServerArgs: args})
		defer tc.Stopper().Stop(ctx)
		tdb := sqlutils.MakeSQLRunner(tc.ServerConn(0))

		qExists := fmt.Sprintf(`
    SELECT recurrence, count(*)
      FROM [SHOW SCHEDULES]
      WHERE label = '%s'
      GROUP BY recurrence`,
			schematelemetrycontroller.SchemaTelemetryScheduleName)

		qJob := fmt.Sprintf(`SELECT %s()`,
			builtinconstants.CreateSchemaTelemetryJobBuiltinName)

		// Check that there is no schema telemetry schedule and that creating schema
		// telemetry jobs is not possible.
		tdb.CheckQueryResults(t, qExists, [][]string{})
		tdb.ExpectErr(t, schematelemetrycontroller.ErrVersionGate.Error(), qJob)

		// Upgrade the cluster.
		tdb.Exec(t, `SET CLUSTER SETTING version = $1`,
			clusterversion.ByKey(clusterversion.TODODelete_V22_2SQLSchemaTelemetryScheduledJobs).String())

		// Check that the schedule now exists and that jobs can be created.
		tdb.Exec(t, qJob)
		tdb.CheckQueryResultsRetry(t, qExists, [][]string{{"@weekly", "1"}})

		// Check that the schedule can have its recurrence altered.
		tdb.Exec(t, fmt.Sprintf(`SET CLUSTER SETTING %s = '* * * * *'`,
			schematelemetrycontroller.SchemaTelemetryRecurrence.Key()))
		tdb.CheckQueryResultsRetry(t, qExists, [][]string{{"* * * * *", "1"}})
		clusterID := tc.Server(0).ExecutorConfig().(sql.ExecutorConfig).NodeInfo.
			LogicalClusterID()
		exp := schematelemetrycontroller.MaybeRewriteCronExpr(clusterID, "@daily")
		tdb.Exec(t, fmt.Sprintf(`SET CLUSTER SETTING %s = '@daily'`,
			schematelemetrycontroller.SchemaTelemetryRecurrence.Key()))
		tdb.CheckQueryResultsRetry(t, qExists, [][]string{{exp, "1"}})
	})

}
