// Copyright 2023 The Cockroach Authors.
//
// Licensed as a CockroachDB Enterprise file under the Cockroach Community
// License (the "License"); you may not use this file except in compliance with
// the License. You may obtain a copy of the License at
//
//     https://github.com/cockroachdb/cockroach/blob/master/licenses/CCL.txt

package tenantcapabilitiesccl

import (
	"context"
	gosql "database/sql"
	"fmt"
	"net/url"
	"testing"

	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/kv/kvclient/rangefeed/rangefeedcache"
	"github.com/cockroachdb/cockroach/pkg/multitenant/tenantcapabilities"
	"github.com/cockroachdb/cockroach/pkg/multitenant/tenantcapabilities/tenantcapabilitieswatcher"
	"github.com/cockroachdb/cockroach/pkg/security/username"
	"github.com/cockroachdb/cockroach/pkg/settings/cluster"
	"github.com/cockroachdb/cockroach/pkg/sql"
	"github.com/cockroachdb/cockroach/pkg/testutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/datapathutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/serverutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/sqlutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/testcluster"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/syncutil"
	"github.com/cockroachdb/datadriven"
	"github.com/cockroachdb/errors"
	"github.com/stretchr/testify/require"
)

// TestDataDriven runs datadriven tests against the entire tenant capabilities
// subsystem, and in doing so, serves as an end-to-end integration test.
//
// Crucially, it keeps track of how up-to-date the in-memory Authorizer state
// is when capabilities are updated. This allows test authors to change
// capabilities and make assertions against those changes, without needing to
// worry about the asynchronous nature of capability changes applying.
//
// The test creates a secondary tenant, with tenant ID 10, that test authors can
// reference directly.
//
// The syntax is as follows:
//
// query-sql-system: runs a query against the system tenant.
//
// exec-sql-tenant: executes a query against a secondary tenant (with ID 10).
//
// exec-privileged-op-tenant: executes a privileged operation (one that requires
// capabilities) as a secondary tenant.
//
// update-capabilities: expects a SQL statement that updates capabilities for a
// tenant. Following statements are guaranteed to see the effects of the
// update.
func TestDataDriven(t *testing.T) {
	defer leaktest.AfterTest(t)()
	datadriven.Walk(t, datapathutils.TestDataPath(t), func(t *testing.T, path string) {
		ctx := context.Background()

		// Setup both the system tenant and a secondary tenant.
		mu := struct {
			syncutil.Mutex
			lastFrontierTS hlc.Timestamp // ensures watcher is caught up
		}{}

		tc := testcluster.StartTestCluster(t, 1, base.TestClusterArgs{
			ServerArgs: base.TestServerArgs{
				DisableDefaultTestTenant: true, // We'll create a tenant ourselves.
				Knobs: base.TestingKnobs{
					TenantCapabilitiesTestingKnobs: &tenantcapabilities.TestingKnobs{
						WatcherTestingKnobs: &tenantcapabilitieswatcher.TestingKnobs{
							WatcherRangeFeedKnobs: &rangefeedcache.TestingKnobs{
								OnTimestampAdvance: func(ts hlc.Timestamp) {
									mu.Lock()
									defer mu.Unlock()
									mu.lastFrontierTS = ts
								},
							},
						},
					},
				},
			},
		})
		defer tc.Stopper().Stop(ctx)
		systemSQLDB := sqlutils.MakeSQLRunner(tc.ServerConn(0))

		// Create a tenant; we also want to allow test writers to issue
		// ALTER TABLE ... SPLIT statements, so configure the settings as such.
		// TODO(knz): Once https://github.com/cockroachdb/cockroach/issues/96512 is
		// resolved, we could override this cluster setting for the secondary tenant
		// using SQL instead of reaching in using this testing knob. One way to do
		// so would be to perform the override for all tenants and only then
		// initializing our test tenant; However, the linked issue above prevents
		// us from being able to do so.
		settings := cluster.MakeTestingClusterSettings()
		sql.SecondaryTenantSplitAtEnabled.Override(ctx, &settings.SV, true)
		tenantArgs := base.TestTenantArgs{
			TenantID: serverutils.TestTenantID(),
			Settings: settings,
		}
		testTenantInterface, err := tc.Server(0).StartTenant(ctx, tenantArgs)
		require.NoError(t, err)

		pgURL, cleanupPGUrl := sqlutils.PGUrl(t, testTenantInterface.SQLAddr(), "Tenant", url.User(username.RootUser))
		tenantSQLDB, err := gosql.Open("postgres", pgURL.String())
		defer func() {
			require.NoError(t, tenantSQLDB.Close())
			defer cleanupPGUrl()
		}()
		require.NoError(t, err)

		var lastUpdateTS hlc.Timestamp
		datadriven.RunTest(t, path, func(t *testing.T, d *datadriven.TestData) string {

			switch d.Cmd {
			case "update-capabilities":
				systemSQLDB.Exec(t, d.Input)
				lastUpdateTS = tc.Server(0).Clock().Now()

			case "exec-privileged-op-tenant":
				testutils.SucceedsSoon(t, func() error {
					mu.Lock()
					defer mu.Unlock()

					if lastUpdateTS.Less(mu.lastFrontierTS) {
						return nil
					}

					return errors.Newf("frontier timestamp (%s) lagging last update (%s)",
						mu.lastFrontierTS.String(), lastUpdateTS.String())
				})
				_, err := tenantSQLDB.Exec(d.Input)
				if err != nil {
					return err.Error()
				}

			case "exec-sql-tenant":
				_, err := tenantSQLDB.Exec(d.Input)
				if err != nil {
					return err.Error()
				}

			case "query-sql-system":
				rows := systemSQLDB.Query(t, d.Input)
				output, err := sqlutils.RowsToDataDrivenOutput(rows)
				require.NoError(t, err)
				return output

			default:
				return fmt.Sprintf("unknown command %s", d.Cmd)
			}
			return "ok"
		})
	})
}
