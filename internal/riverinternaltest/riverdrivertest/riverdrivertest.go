package riverdrivertest

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"golang.org/x/text/cases"
	"golang.org/x/text/language"

	"github.com/riverqueue/river/internal/dbunique"
	"github.com/riverqueue/river/internal/notifier"
	"github.com/riverqueue/river/internal/rivercommon"
	"github.com/riverqueue/river/riverdriver"
	"github.com/riverqueue/river/rivershared/testfactory"
	"github.com/riverqueue/river/rivershared/util/ptrutil"
	"github.com/riverqueue/river/rivershared/util/sliceutil"
	"github.com/riverqueue/river/rivertype"
)

// Exercise fully exercises a driver. The driver's listener is exercised if
// supported.
func Exercise[TTx any](ctx context.Context, t *testing.T,
	driverWithSchema func(ctx context.Context, t *testing.T) (riverdriver.Driver[TTx], string),
	executorWithTx func(ctx context.Context, t *testing.T) riverdriver.Executor,
) {
	t.Helper()

	{
		driver, _ := driverWithSchema(ctx, t)
		if driver.SupportsListener() {
			exerciseListener(ctx, t, driverWithSchema)
		} else {
			t.Logf("Driver does not support listener; skipping listener tests")
		}
	}

	t.Run("GetMigrationFS", func(t *testing.T) {
		t.Parallel()

		driver, _ := driverWithSchema(ctx, t)

		for _, line := range driver.GetMigrationLines() {
			migrationFS := driver.GetMigrationFS(line)

			// Directory for the advertised migration line should exist.
			_, err := migrationFS.Open("migration/" + line)
			require.NoError(t, err)
		}
	})

	t.Run("GetMigrationTruncateTables", func(t *testing.T) {
		t.Parallel()

		driver, _ := driverWithSchema(ctx, t)

		for _, line := range driver.GetMigrationLines() {
			truncateTables := driver.GetMigrationTruncateTables(line)

			// Technically a migration line's truncate tables might be empty,
			// but this never happens in any of our migration lines, so check
			// non-empty until it becomes an actual problem.
			require.NotEmpty(t, truncateTables)
		}
	})

	t.Run("GetMigrationLines", func(t *testing.T) {
		t.Parallel()

		driver, _ := driverWithSchema(ctx, t)

		// Should contain at minimum a main migration line.
		require.Contains(t, driver.GetMigrationLines(), riverdriver.MigrationLineMain)
	})

	type testBundle struct{}

	setup := func(ctx context.Context, t *testing.T) (riverdriver.Executor, *testBundle) {
		t.Helper()
		return executorWithTx(ctx, t), &testBundle{}
	}

	const clientID = "test-client-id"

	t.Run("Begin", func(t *testing.T) {
		t.Parallel()

		t.Run("BasicVisibility", func(t *testing.T) {
			t.Parallel()

			exec, _ := setup(ctx, t)

			tx, err := exec.Begin(ctx)
			require.NoError(t, err)
			t.Cleanup(func() { _ = tx.Rollback(ctx) })

			// Job visible in subtransaction, but not parent.
			{
				job := testfactory.Job(ctx, t, tx, &testfactory.JobOpts{})
				_ = testfactory.Job(ctx, t, tx, &testfactory.JobOpts{})

				_, err := tx.JobGetByID(ctx, &riverdriver.JobGetByIDParams{ID: job.ID, Schema: ""})
				require.NoError(t, err)

				require.NoError(t, tx.Rollback(ctx))

				_, err = exec.JobGetByID(ctx, &riverdriver.JobGetByIDParams{ID: job.ID, Schema: ""})
				require.ErrorIs(t, err, rivertype.ErrNotFound)
			}
		})

		t.Run("NestedTransactions", func(t *testing.T) {
			t.Parallel()

			exec, _ := setup(ctx, t)

			tx1, err := exec.Begin(ctx)
			require.NoError(t, err)
			t.Cleanup(func() { _ = tx1.Rollback(ctx) })

			// Job visible in tx1, but not top level executor.
			{
				job1 := testfactory.Job(ctx, t, tx1, &testfactory.JobOpts{})

				{
					tx2, err := tx1.Begin(ctx)
					require.NoError(t, err)
					t.Cleanup(func() { _ = tx2.Rollback(ctx) })

					// Job visible in tx2, but not top level executor.
					{
						job2 := testfactory.Job(ctx, t, tx2, &testfactory.JobOpts{})

						_, err := tx2.JobGetByID(ctx, &riverdriver.JobGetByIDParams{ID: job2.ID, Schema: ""})
						require.NoError(t, err)

						require.NoError(t, tx2.Rollback(ctx))

						_, err = tx1.JobGetByID(ctx, &riverdriver.JobGetByIDParams{ID: job2.ID, Schema: ""})
						require.ErrorIs(t, err, rivertype.ErrNotFound)
					}

					_, err = tx1.JobGetByID(ctx, &riverdriver.JobGetByIDParams{ID: job1.ID, Schema: ""})
					require.NoError(t, err)
				}

				// Repeat the same subtransaction again.
				{
					tx2, err := tx1.Begin(ctx)
					require.NoError(t, err)
					t.Cleanup(func() { _ = tx2.Rollback(ctx) })

					// Job visible in tx2, but not top level executor.
					{
						job2 := testfactory.Job(ctx, t, tx2, &testfactory.JobOpts{})

						_, err = tx2.JobGetByID(ctx, &riverdriver.JobGetByIDParams{ID: job2.ID, Schema: ""})
						require.NoError(t, err)

						require.NoError(t, tx2.Rollback(ctx))

						_, err = tx1.JobGetByID(ctx, &riverdriver.JobGetByIDParams{ID: job2.ID, Schema: ""})
						require.ErrorIs(t, err, rivertype.ErrNotFound)
					}

					_, err = tx1.JobGetByID(ctx, &riverdriver.JobGetByIDParams{ID: job1.ID, Schema: ""})
					require.NoError(t, err)
				}

				require.NoError(t, tx1.Rollback(ctx))

				_, err = exec.JobGetByID(ctx, &riverdriver.JobGetByIDParams{ID: job1.ID, Schema: ""})
				require.ErrorIs(t, err, rivertype.ErrNotFound)
			}
		})

		t.Run("RollbackAfterCommit", func(t *testing.T) {
			t.Parallel()

			exec, _ := setup(ctx, t)

			tx1, err := exec.Begin(ctx)
			require.NoError(t, err)
			t.Cleanup(func() { _ = tx1.Rollback(ctx) })

			tx2, err := tx1.Begin(ctx)
			require.NoError(t, err)
			t.Cleanup(func() { _ = tx2.Rollback(ctx) })

			job := testfactory.Job(ctx, t, tx2, &testfactory.JobOpts{})

			require.NoError(t, tx2.Commit(ctx))
			_ = tx2.Rollback(ctx) // "tx is closed" error generally returned, but don't require this

			// Despite rollback being called after commit, the job is still
			// visible from the outer transaction.
			_, err = tx1.JobGetByID(ctx, &riverdriver.JobGetByIDParams{ID: job.ID, Schema: ""})
			require.NoError(t, err)
		})
	})

	t.Run("ColumnExists", func(t *testing.T) {
		t.Parallel()

		exec, _ := setup(ctx, t)

		exists, err := exec.ColumnExists(ctx, &riverdriver.ColumnExistsParams{
			Column: "id",
			Table:  "river_job",
		})
		require.NoError(t, err)
		require.True(t, exists)

		exists, err = exec.ColumnExists(ctx, &riverdriver.ColumnExistsParams{
			Column: "does_not_exist",
			Table:  "river_job",
		})
		require.NoError(t, err)
		require.False(t, exists)

		exists, err = exec.ColumnExists(ctx, &riverdriver.ColumnExistsParams{
			Column: "id",
			Table:  "does_not_exist",
		})
		require.NoError(t, err)
		require.False(t, exists)

		// Will be rolled back by the test transaction.
		_, err = exec.Exec(ctx, "CREATE SCHEMA another_schema_123")
		require.NoError(t, err)

		_, err = exec.Exec(ctx, "SET search_path = another_schema_123")
		require.NoError(t, err)

		// Table with the same name as the main schema, but without the same
		// columns.
		_, err = exec.Exec(ctx, "CREATE TABLE river_job (another_id bigint)")
		require.NoError(t, err)

		exists, err = exec.ColumnExists(ctx, &riverdriver.ColumnExistsParams{
			Column: "id",
			Table:  "river_job",
		})
		require.NoError(t, err)
		require.False(t, exists)
	})

	t.Run("Exec", func(t *testing.T) {
		t.Parallel()

		exec, _ := setup(ctx, t)

		_, err := exec.Exec(ctx, "SELECT 1 + 2")
		require.NoError(t, err)
	})

	t.Run("JobCancel", func(t *testing.T) {
		t.Parallel()

		for _, startingState := range []rivertype.JobState{
			rivertype.JobStateAvailable,
			rivertype.JobStateRetryable,
			rivertype.JobStateScheduled,
		} {
			t.Run(fmt.Sprintf("CancelsJobIn%sState", startingState), func(t *testing.T) {
				t.Parallel()

				exec, _ := setup(ctx, t)

				now := time.Now().UTC()
				nowStr := now.Format(time.RFC3339Nano)

				job := testfactory.Job(ctx, t, exec, &testfactory.JobOpts{
					State:     &startingState,
					UniqueKey: []byte("unique-key"),
				})
				require.Equal(t, startingState, job.State)

				jobAfter, err := exec.JobCancel(ctx, &riverdriver.JobCancelParams{
					ID:                job.ID,
					CancelAttemptedAt: now,
					ControlTopic:      string(notifier.NotificationTopicControl),
				})
				require.NoError(t, err)
				require.NotNil(t, jobAfter)

				require.Equal(t, rivertype.JobStateCancelled, jobAfter.State)
				require.WithinDuration(t, time.Now(), *jobAfter.FinalizedAt, 2*time.Second)
				require.JSONEq(t, fmt.Sprintf(`{"cancel_attempted_at":%q}`, nowStr), string(jobAfter.Metadata))
			})
		}

		t.Run("RunningJobIsNotImmediatelyCancelled", func(t *testing.T) {
			t.Parallel()

			exec, _ := setup(ctx, t)

			now := time.Now().UTC()
			nowStr := now.Format(time.RFC3339Nano)

			job := testfactory.Job(ctx, t, exec, &testfactory.JobOpts{
				State:     ptrutil.Ptr(rivertype.JobStateRunning),
				UniqueKey: []byte("unique-key"),
			})
			require.Equal(t, rivertype.JobStateRunning, job.State)

			jobAfter, err := exec.JobCancel(ctx, &riverdriver.JobCancelParams{
				ID:                job.ID,
				CancelAttemptedAt: now,
				ControlTopic:      string(notifier.NotificationTopicControl),
			})
			require.NoError(t, err)
			require.NotNil(t, jobAfter)
			require.Equal(t, rivertype.JobStateRunning, jobAfter.State)
			require.Nil(t, jobAfter.FinalizedAt)
			require.JSONEq(t, fmt.Sprintf(`{"cancel_attempted_at":%q}`, nowStr), string(jobAfter.Metadata))
			require.Equal(t, "unique-key", string(jobAfter.UniqueKey))
		})

		for _, startingState := range []rivertype.JobState{
			rivertype.JobStateCancelled,
			rivertype.JobStateCompleted,
			rivertype.JobStateDiscarded,
		} {
			t.Run(fmt.Sprintf("DoesNotAlterFinalizedJobIn%sState", startingState), func(t *testing.T) {
				t.Parallel()

				exec, _ := setup(ctx, t)

				job := testfactory.Job(ctx, t, exec, &testfactory.JobOpts{
					FinalizedAt: ptrutil.Ptr(time.Now()),
					State:       &startingState,
				})

				jobAfter, err := exec.JobCancel(ctx, &riverdriver.JobCancelParams{
					ID:                job.ID,
					CancelAttemptedAt: time.Now(),
					ControlTopic:      string(notifier.NotificationTopicControl),
				})
				require.NoError(t, err)
				require.Equal(t, startingState, jobAfter.State)
				require.WithinDuration(t, *job.FinalizedAt, *jobAfter.FinalizedAt, time.Microsecond)
				require.JSONEq(t, `{}`, string(jobAfter.Metadata))
			})
		}

		t.Run("ReturnsErrNotFoundIfJobDoesNotExist", func(t *testing.T) {
			t.Parallel()

			exec, _ := setup(ctx, t)

			jobAfter, err := exec.JobCancel(ctx, &riverdriver.JobCancelParams{
				ID:                1234567890,
				CancelAttemptedAt: time.Now(),
				ControlTopic:      string(notifier.NotificationTopicControl),
			})
			require.ErrorIs(t, err, rivertype.ErrNotFound)
			require.Nil(t, jobAfter)
		})
	})

	t.Run("JobCountByState", func(t *testing.T) {
		t.Parallel()

		t.Run("CountsJobsByState", func(t *testing.T) {
			t.Parallel()

			exec, _ := setup(ctx, t)

			// Included because they're the queried state.
			_ = testfactory.Job(ctx, t, exec, &testfactory.JobOpts{State: ptrutil.Ptr(rivertype.JobStateAvailable)})
			_ = testfactory.Job(ctx, t, exec, &testfactory.JobOpts{State: ptrutil.Ptr(rivertype.JobStateAvailable)})

			// Excluded because they're not.
			finalizedAt := ptrutil.Ptr(time.Now())
			_ = testfactory.Job(ctx, t, exec, &testfactory.JobOpts{FinalizedAt: finalizedAt, State: ptrutil.Ptr(rivertype.JobStateCancelled)})
			_ = testfactory.Job(ctx, t, exec, &testfactory.JobOpts{FinalizedAt: finalizedAt, State: ptrutil.Ptr(rivertype.JobStateCompleted)})
			_ = testfactory.Job(ctx, t, exec, &testfactory.JobOpts{FinalizedAt: finalizedAt, State: ptrutil.Ptr(rivertype.JobStateDiscarded)})

			numJobs, err := exec.JobCountByState(ctx, &riverdriver.JobCountByStateParams{
				State: rivertype.JobStateAvailable,
			})
			require.NoError(t, err)
			require.Equal(t, 2, numJobs)
		})

		t.Run("AlternateSchema", func(t *testing.T) {
			t.Parallel()

			exec, _ := setup(ctx, t)

			_, err := exec.JobCountByState(ctx, &riverdriver.JobCountByStateParams{
				Schema: "custom_schema",
				State:  rivertype.JobStateAvailable,
			})
			requireMissingRelation(t, err, "custom_schema.river_job")
		})
	})

	t.Run("JobDelete", func(t *testing.T) {
		t.Parallel()

		t.Run("DoesNotDeleteARunningJob", func(t *testing.T) {
			t.Parallel()

			exec, _ := setup(ctx, t)

			job := testfactory.Job(ctx, t, exec, &testfactory.JobOpts{
				State: ptrutil.Ptr(rivertype.JobStateRunning),
			})

			jobAfter, err := exec.JobDelete(ctx, &riverdriver.JobDeleteParams{
				ID: job.ID,
			})
			require.ErrorIs(t, err, rivertype.ErrJobRunning)
			require.Nil(t, jobAfter)

			jobUpdated, err := exec.JobGetByID(ctx, &riverdriver.JobGetByIDParams{ID: job.ID, Schema: ""})
			require.NoError(t, err)
			require.Equal(t, rivertype.JobStateRunning, jobUpdated.State)
		})

		for _, state := range []rivertype.JobState{
			rivertype.JobStateAvailable,
			rivertype.JobStateCancelled,
			rivertype.JobStateCompleted,
			rivertype.JobStateDiscarded,
			rivertype.JobStatePending,
			rivertype.JobStateRetryable,
			rivertype.JobStateScheduled,
		} {
			t.Run(fmt.Sprintf("DeletesA_%s_Job", state), func(t *testing.T) {
				t.Parallel()

				exec, _ := setup(ctx, t)

				now := time.Now().UTC()

				setFinalized := slices.Contains([]rivertype.JobState{
					rivertype.JobStateCancelled,
					rivertype.JobStateCompleted,
					rivertype.JobStateDiscarded,
				}, state)

				var finalizedAt *time.Time
				if setFinalized {
					finalizedAt = &now
				}

				job := testfactory.Job(ctx, t, exec, &testfactory.JobOpts{
					FinalizedAt: finalizedAt,
					ScheduledAt: ptrutil.Ptr(now.Add(1 * time.Hour)),
					State:       &state,
				})

				jobAfter, err := exec.JobDelete(ctx, &riverdriver.JobDeleteParams{
					ID:     job.ID,
					Schema: "",
				})
				require.NoError(t, err)
				require.NotNil(t, jobAfter)
				require.Equal(t, job.ID, jobAfter.ID)
				require.Equal(t, state, jobAfter.State)

				_, err = exec.JobGetByID(ctx, &riverdriver.JobGetByIDParams{ID: job.ID, Schema: ""})
				require.ErrorIs(t, err, rivertype.ErrNotFound)
			})
		}

		t.Run("ReturnsErrNotFoundIfJobDoesNotExist", func(t *testing.T) {
			t.Parallel()

			exec, _ := setup(ctx, t)

			jobAfter, err := exec.JobDelete(ctx, &riverdriver.JobDeleteParams{
				ID: 1234567890,
			})
			require.ErrorIs(t, err, rivertype.ErrNotFound)
			require.Nil(t, jobAfter)
		})

		t.Run("AlternateSchema", func(t *testing.T) {
			t.Parallel()

			exec, _ := setup(ctx, t)

			job := testfactory.Job(ctx, t, exec, &testfactory.JobOpts{})

			_, err := exec.JobDelete(ctx, &riverdriver.JobDeleteParams{
				ID:     job.ID,
				Schema: "custom_schema",
			})
			requireMissingRelation(t, err, "custom_schema.river_job")
		})
	})

	t.Run("JobDeleteBefore", func(t *testing.T) {
		t.Parallel()

		exec, _ := setup(ctx, t)

		var (
			horizon       = time.Now()
			beforeHorizon = horizon.Add(-1 * time.Minute)
			afterHorizon  = horizon.Add(1 * time.Minute)
		)

		deletedJob1 := testfactory.Job(ctx, t, exec, &testfactory.JobOpts{FinalizedAt: &beforeHorizon, State: ptrutil.Ptr(rivertype.JobStateCancelled)})
		deletedJob2 := testfactory.Job(ctx, t, exec, &testfactory.JobOpts{FinalizedAt: &beforeHorizon, State: ptrutil.Ptr(rivertype.JobStateCompleted)})
		deletedJob3 := testfactory.Job(ctx, t, exec, &testfactory.JobOpts{FinalizedAt: &beforeHorizon, State: ptrutil.Ptr(rivertype.JobStateDiscarded)})

		// Not deleted because not appropriate state.
		notDeletedJob1 := testfactory.Job(ctx, t, exec, &testfactory.JobOpts{State: ptrutil.Ptr(rivertype.JobStateAvailable)})
		notDeletedJob2 := testfactory.Job(ctx, t, exec, &testfactory.JobOpts{State: ptrutil.Ptr(rivertype.JobStateRunning)})

		// Not deleted because after the delete horizon.
		notDeletedJob3 := testfactory.Job(ctx, t, exec, &testfactory.JobOpts{FinalizedAt: &afterHorizon, State: ptrutil.Ptr(rivertype.JobStateCancelled)})

		// Max two deleted on the first pass.
		numDeleted, err := exec.JobDeleteBefore(ctx, &riverdriver.JobDeleteBeforeParams{
			CancelledFinalizedAtHorizon: horizon,
			CompletedFinalizedAtHorizon: horizon,
			DiscardedFinalizedAtHorizon: horizon,
			Max:                         2,
		})
		require.NoError(t, err)
		require.Equal(t, 2, numDeleted)

		// And one more pass gets the last one.
		numDeleted, err = exec.JobDeleteBefore(ctx, &riverdriver.JobDeleteBeforeParams{
			CancelledFinalizedAtHorizon: horizon,
			CompletedFinalizedAtHorizon: horizon,
			DiscardedFinalizedAtHorizon: horizon,
			Max:                         2,
		})
		require.NoError(t, err)
		require.Equal(t, 1, numDeleted)

		// All deleted.
		_, err = exec.JobGetByID(ctx, &riverdriver.JobGetByIDParams{ID: deletedJob1.ID, Schema: ""})
		require.ErrorIs(t, err, rivertype.ErrNotFound)
		_, err = exec.JobGetByID(ctx, &riverdriver.JobGetByIDParams{ID: deletedJob2.ID, Schema: ""})
		require.ErrorIs(t, err, rivertype.ErrNotFound)
		_, err = exec.JobGetByID(ctx, &riverdriver.JobGetByIDParams{ID: deletedJob3.ID, Schema: ""})
		require.ErrorIs(t, err, rivertype.ErrNotFound)

		// Not deleted
		_, err = exec.JobGetByID(ctx, &riverdriver.JobGetByIDParams{ID: notDeletedJob1.ID, Schema: ""})
		require.NoError(t, err)
		_, err = exec.JobGetByID(ctx, &riverdriver.JobGetByIDParams{ID: notDeletedJob2.ID, Schema: ""})
		require.NoError(t, err)
		_, err = exec.JobGetByID(ctx, &riverdriver.JobGetByIDParams{ID: notDeletedJob3.ID, Schema: ""})
		require.NoError(t, err)
	})

	t.Run("JobGetAvailable", func(t *testing.T) {
		t.Parallel()

		t.Run("Success", func(t *testing.T) {
			t.Parallel()

			exec, _ := setup(ctx, t)

			_ = testfactory.Job(ctx, t, exec, &testfactory.JobOpts{})

			jobRows, err := exec.JobGetAvailable(ctx, &riverdriver.JobGetAvailableParams{
				ClientID: clientID,
				Max:      100,
				Queue:    rivercommon.QueueDefault,
			})
			require.NoError(t, err)
			require.Len(t, jobRows, 1)

			jobRow := jobRows[0]
			require.Equal(t, []string{clientID}, jobRow.AttemptedBy)
		})

		t.Run("ConstrainedToLimit", func(t *testing.T) {
			t.Parallel()

			exec, _ := setup(ctx, t)

			_ = testfactory.Job(ctx, t, exec, &testfactory.JobOpts{})
			_ = testfactory.Job(ctx, t, exec, &testfactory.JobOpts{})

			// Two rows inserted but only one found because of the added limit.
			jobRows, err := exec.JobGetAvailable(ctx, &riverdriver.JobGetAvailableParams{
				ClientID: clientID,
				Max:      1,
				Queue:    rivercommon.QueueDefault,
			})
			require.NoError(t, err)
			require.Len(t, jobRows, 1)
		})

		t.Run("ConstrainedToQueue", func(t *testing.T) {
			t.Parallel()

			exec, _ := setup(ctx, t)

			_ = testfactory.Job(ctx, t, exec, &testfactory.JobOpts{
				Queue: ptrutil.Ptr("other-queue"),
			})

			// Job is in a non-default queue so it's not found.
			jobRows, err := exec.JobGetAvailable(ctx, &riverdriver.JobGetAvailableParams{
				ClientID: clientID,
				Max:      100,
				Queue:    rivercommon.QueueDefault,
			})
			require.NoError(t, err)
			require.Empty(t, jobRows)
		})

		t.Run("ConstrainedToScheduledAtBeforeNow", func(t *testing.T) {
			t.Parallel()

			exec, _ := setup(ctx, t)

			_ = testfactory.Job(ctx, t, exec, &testfactory.JobOpts{
				ScheduledAt: ptrutil.Ptr(time.Now().Add(1 * time.Minute)),
			})

			// Job is scheduled a while from now so it's not found.
			jobRows, err := exec.JobGetAvailable(ctx, &riverdriver.JobGetAvailableParams{
				ClientID: clientID,
				Max:      100,
				Queue:    rivercommon.QueueDefault,
			})
			require.NoError(t, err)
			require.Empty(t, jobRows)
		})

		t.Run("ConstrainedToScheduledAtBeforeCustomNowTime", func(t *testing.T) {
			t.Parallel()

			exec, _ := setup(ctx, t)

			now := time.Now().Add(1 * time.Minute)
			// Job 1 is scheduled after now so it's not found:
			_ = testfactory.Job(ctx, t, exec, &testfactory.JobOpts{
				ScheduledAt: ptrutil.Ptr(now.Add(1 * time.Minute)),
			})
			// Job 2 is scheduled just before now so it's found:
			job2 := testfactory.Job(ctx, t, exec, &testfactory.JobOpts{
				ScheduledAt: ptrutil.Ptr(now.Add(-1 * time.Microsecond)),
			})

			jobRows, err := exec.JobGetAvailable(ctx, &riverdriver.JobGetAvailableParams{
				ClientID: clientID,
				Max:      100,
				Now:      ptrutil.Ptr(now),
				Queue:    rivercommon.QueueDefault,
			})
			require.NoError(t, err)
			require.Len(t, jobRows, 1)
			require.Equal(t, job2.ID, jobRows[0].ID)
		})

		t.Run("Prioritized", func(t *testing.T) {
			t.Parallel()

			exec, _ := setup(ctx, t)

			// Insert jobs with decreasing priority numbers (3, 2, 1) which means increasing priority.
			for i := 3; i > 0; i-- {
				_ = testfactory.Job(ctx, t, exec, &testfactory.JobOpts{
					Priority: &i,
				})
			}

			jobRows, err := exec.JobGetAvailable(ctx, &riverdriver.JobGetAvailableParams{
				ClientID: clientID,
				Max:      2,
				Queue:    rivercommon.QueueDefault,
			})
			require.NoError(t, err)
			require.Len(t, jobRows, 2, "expected to fetch exactly 2 jobs")

			// Because the jobs are ordered within the fetch query's CTE but *not* within
			// the final query, the final result list may not actually be sorted. This is
			// fine, because we've already ensured that we've fetched the jobs we wanted
			// to fetch via that ORDER BY. For testing we'll need to sort the list after
			// fetch to easily assert that the expected jobs are in it.
			sort.Slice(jobRows, func(i, j int) bool { return jobRows[i].Priority < jobRows[j].Priority })

			require.Equal(t, 1, jobRows[0].Priority, "expected first job to have priority 1")
			require.Equal(t, 2, jobRows[1].Priority, "expected second job to have priority 2")

			// Should fetch the one remaining job on the next attempt:
			jobRows, err = exec.JobGetAvailable(ctx, &riverdriver.JobGetAvailableParams{
				ClientID: clientID,
				Max:      1,
				Queue:    rivercommon.QueueDefault,
			})
			require.NoError(t, err)
			require.NoError(t, err)
			require.Len(t, jobRows, 1, "expected to fetch exactly 1 job")
			require.Equal(t, 3, jobRows[0].Priority, "expected final job to have priority 3")
		})
	})

	t.Run("JobGetByID", func(t *testing.T) {
		t.Parallel()

		t.Run("FetchesAnExistingJob", func(t *testing.T) {
			t.Parallel()

			exec, _ := setup(ctx, t)

			now := time.Now().UTC()

			job := testfactory.Job(ctx, t, exec, &testfactory.JobOpts{})

			fetchedJob, err := exec.JobGetByID(ctx, &riverdriver.JobGetByIDParams{ID: job.ID, Schema: ""})
			require.NoError(t, err)
			require.NotNil(t, fetchedJob)

			require.Equal(t, job.ID, fetchedJob.ID)
			require.Equal(t, rivertype.JobStateAvailable, fetchedJob.State)
			require.WithinDuration(t, now, fetchedJob.CreatedAt, 100*time.Millisecond)
			require.WithinDuration(t, now, fetchedJob.ScheduledAt, 100*time.Millisecond)
		})

		t.Run("ReturnsErrNotFoundIfJobDoesNotExist", func(t *testing.T) {
			t.Parallel()

			exec, _ := setup(ctx, t)

			job, err := exec.JobGetByID(ctx, &riverdriver.JobGetByIDParams{ID: 0, Schema: ""})
			require.Error(t, err)
			require.ErrorIs(t, err, rivertype.ErrNotFound)
			require.Nil(t, job)
		})
	})

	t.Run("JobGetByIDMany", func(t *testing.T) {
		t.Parallel()

		exec, _ := setup(ctx, t)

		job1 := testfactory.Job(ctx, t, exec, &testfactory.JobOpts{})
		job2 := testfactory.Job(ctx, t, exec, &testfactory.JobOpts{})

		// Not returned.
		_ = testfactory.Job(ctx, t, exec, &testfactory.JobOpts{})

		jobs, err := exec.JobGetByIDMany(ctx, &riverdriver.JobGetByIDManyParams{
			ID: []int64{job1.ID, job2.ID},
		})
		require.NoError(t, err)
		require.Equal(t, []int64{job1.ID, job2.ID},
			sliceutil.Map(jobs, func(j *rivertype.JobRow) int64 { return j.ID }))
	})

	t.Run("JobGetByKindMany", func(t *testing.T) {
		t.Parallel()

		exec, _ := setup(ctx, t)

		job1 := testfactory.Job(ctx, t, exec, &testfactory.JobOpts{Kind: ptrutil.Ptr("kind1")})
		job2 := testfactory.Job(ctx, t, exec, &testfactory.JobOpts{Kind: ptrutil.Ptr("kind2")})

		// Not returned.
		_ = testfactory.Job(ctx, t, exec, &testfactory.JobOpts{Kind: ptrutil.Ptr("kind3")})

		jobs, err := exec.JobGetByKindMany(ctx, &riverdriver.JobGetByKindManyParams{
			Kind:   []string{job1.Kind, job2.Kind},
			Schema: "",
		})
		require.NoError(t, err)
		require.Equal(t, []int64{job1.ID, job2.ID},
			sliceutil.Map(jobs, func(j *rivertype.JobRow) int64 { return j.ID }))
	})

	t.Run("JobGetStuck", func(t *testing.T) {
		t.Parallel()

		exec, _ := setup(ctx, t)

		var (
			horizon       = time.Now()
			beforeHorizon = horizon.Add(-1 * time.Minute)
			afterHorizon  = horizon.Add(1 * time.Minute)
		)

		stuckJob1 := testfactory.Job(ctx, t, exec, &testfactory.JobOpts{AttemptedAt: &beforeHorizon, State: ptrutil.Ptr(rivertype.JobStateRunning)})
		stuckJob2 := testfactory.Job(ctx, t, exec, &testfactory.JobOpts{AttemptedAt: &beforeHorizon, State: ptrutil.Ptr(rivertype.JobStateRunning)})

		// Not returned because we put a maximum of two.
		_ = testfactory.Job(ctx, t, exec, &testfactory.JobOpts{AttemptedAt: &beforeHorizon, State: ptrutil.Ptr(rivertype.JobStateRunning)})

		// Not stuck because not in running state.
		_ = testfactory.Job(ctx, t, exec, &testfactory.JobOpts{State: ptrutil.Ptr(rivertype.JobStateAvailable)})

		// Not stuck because after queried horizon.
		_ = testfactory.Job(ctx, t, exec, &testfactory.JobOpts{AttemptedAt: &afterHorizon, State: ptrutil.Ptr(rivertype.JobStateRunning)})

		// Max two stuck
		stuckJobs, err := exec.JobGetStuck(ctx, &riverdriver.JobGetStuckParams{
			StuckHorizon: horizon,
			Max:          2,
		})
		require.NoError(t, err)
		require.Equal(t, []int64{stuckJob1.ID, stuckJob2.ID},
			sliceutil.Map(stuckJobs, func(j *rivertype.JobRow) int64 { return j.ID }))
	})

	t.Run("JobInsertFastMany", func(t *testing.T) {
		t.Parallel()

		t.Run("AllArgs", func(t *testing.T) {
			t.Parallel()

			exec, _ := setup(ctx, t)

			now := time.Now().UTC()

			insertParams := make([]*riverdriver.JobInsertFastParams, 10)
			for i := 0; i < len(insertParams); i++ {
				insertParams[i] = &riverdriver.JobInsertFastParams{
					CreatedAt:    ptrutil.Ptr(now.Add(time.Duration(i) * 5 * time.Second)),
					EncodedArgs:  []byte(`{"encoded": "args"}`),
					Kind:         "test_kind",
					MaxAttempts:  rivercommon.MaxAttemptsDefault,
					Metadata:     []byte(`{"meta": "data"}`),
					Priority:     rivercommon.PriorityDefault,
					Queue:        rivercommon.QueueDefault,
					ScheduledAt:  ptrutil.Ptr(now.Add(time.Duration(i) * time.Minute)),
					State:        rivertype.JobStateAvailable,
					Tags:         []string{"tag"},
					UniqueKey:    []byte("unique-key-fast-many-" + strconv.Itoa(i)),
					UniqueStates: 0xff,
				}
			}

			resultRows, err := exec.JobInsertFastMany(ctx, &riverdriver.JobInsertFastManyParams{
				Jobs: insertParams,
			})
			require.NoError(t, err)
			require.Len(t, resultRows, len(insertParams))

			for i, result := range resultRows {
				require.False(t, result.UniqueSkippedAsDuplicate)
				job := result.Job
				require.Equal(t, 0, job.Attempt)
				require.Nil(t, job.AttemptedAt)
				require.Empty(t, job.AttemptedBy)
				require.WithinDuration(t, now.Add(time.Duration(i)*5*time.Second), job.CreatedAt, time.Millisecond)
				require.JSONEq(t, `{"encoded": "args"}`, string(job.EncodedArgs))
				require.Empty(t, job.Errors)
				require.Nil(t, job.FinalizedAt)
				require.Equal(t, "test_kind", job.Kind)
				require.Equal(t, rivercommon.MaxAttemptsDefault, job.MaxAttempts)
				require.JSONEq(t, `{"meta": "data"}`, string(job.Metadata))
				require.Equal(t, rivercommon.PriorityDefault, job.Priority)
				require.Equal(t, rivercommon.QueueDefault, job.Queue)
				requireEqualTime(t, now.Add(time.Duration(i)*time.Minute), job.ScheduledAt)
				require.Equal(t, rivertype.JobStateAvailable, job.State)
				require.Equal(t, []string{"tag"}, job.Tags)
				require.Equal(t, []byte("unique-key-fast-many-"+strconv.Itoa(i)), job.UniqueKey)
				require.Equal(t, rivertype.JobStates(), job.UniqueStates)
			}
		})

		t.Run("MissingValuesDefaultAsExpected", func(t *testing.T) {
			t.Parallel()

			exec, _ := setup(ctx, t)

			insertParams := make([]*riverdriver.JobInsertFastParams, 10)
			for i := 0; i < len(insertParams); i++ {
				insertParams[i] = &riverdriver.JobInsertFastParams{
					EncodedArgs:  []byte(`{"encoded": "args"}`),
					Kind:         "test_kind",
					MaxAttempts:  rivercommon.MaxAttemptsDefault,
					Metadata:     []byte(`{"meta": "data"}`),
					Priority:     rivercommon.PriorityDefault,
					Queue:        rivercommon.QueueDefault,
					ScheduledAt:  nil, // explicit nil
					State:        rivertype.JobStateAvailable,
					Tags:         []string{"tag"},
					UniqueKey:    nil,  // explicit nil
					UniqueStates: 0x00, // explicit 0
				}
			}

			results, err := exec.JobInsertFastMany(ctx, &riverdriver.JobInsertFastManyParams{
				Jobs:   insertParams,
				Schema: "",
			})
			require.NoError(t, err)
			require.Len(t, results, len(insertParams))

			jobsAfter, err := exec.JobGetByKindMany(ctx, &riverdriver.JobGetByKindManyParams{
				Kind:   []string{"test_kind"},
				Schema: "",
			})
			require.NoError(t, err)
			require.Len(t, jobsAfter, len(insertParams))
			for _, job := range jobsAfter {
				require.WithinDuration(t, time.Now().UTC(), job.CreatedAt, 2*time.Second)
				require.WithinDuration(t, time.Now().UTC(), job.ScheduledAt, 2*time.Second)

				// UniqueKey and UniqueStates are not set in the insert params, so they should
				// be nil and an empty slice respectively.
				require.Nil(t, job.UniqueKey)
				var emptyJobStates []rivertype.JobState
				require.Equal(t, emptyJobStates, job.UniqueStates)
			}
		})

		t.Run("BinaryNonUTF8UniqueKey", func(t *testing.T) {
			t.Parallel()

			exec, _ := setup(ctx, t)

			uniqueKey := []byte{0x00, 0x01, 0x02}
			results, err := exec.JobInsertFastMany(ctx, &riverdriver.JobInsertFastManyParams{
				Jobs: []*riverdriver.JobInsertFastParams{
					{
						EncodedArgs:  []byte(`{"encoded": "args"}`),
						Kind:         "test_kind",
						MaxAttempts:  rivercommon.MaxAttemptsDefault,
						Metadata:     []byte(`{"meta": "data"}`),
						Priority:     rivercommon.PriorityDefault,
						Queue:        rivercommon.QueueDefault,
						ScheduledAt:  nil, // explicit nil
						State:        rivertype.JobStateAvailable,
						Tags:         []string{"tag"},
						UniqueKey:    uniqueKey,
						UniqueStates: 0xff,
					},
				},
			})
			require.NoError(t, err)
			require.Len(t, results, 1)
			require.Equal(t, uniqueKey, results[0].Job.UniqueKey)

			jobs, err := exec.JobGetByKindMany(ctx, &riverdriver.JobGetByKindManyParams{
				Kind:   []string{"test_kind"},
				Schema: "",
			})
			require.NoError(t, err)
			require.Equal(t, uniqueKey, jobs[0].UniqueKey)
		})
	})

	t.Run("JobInsertFastManyNoReturning", func(t *testing.T) {
		t.Parallel()

		t.Run("AllArgs", func(t *testing.T) {
			exec, _ := setup(ctx, t)

			// This test needs to use a time from before the transaction begins, otherwise
			// the newly-scheduled jobs won't yet show as available because their
			// scheduled_at (which gets a default value from time.Now() in code) will be
			// after the start of the transaction.
			now := time.Now().UTC().Add(-1 * time.Minute)

			insertParams := make([]*riverdriver.JobInsertFastParams, 10)
			for i := 0; i < len(insertParams); i++ {
				insertParams[i] = &riverdriver.JobInsertFastParams{
					CreatedAt:    ptrutil.Ptr(now.Add(time.Duration(i) * 5 * time.Second)),
					EncodedArgs:  []byte(`{"encoded": "args"}`),
					Kind:         "test_kind",
					MaxAttempts:  rivercommon.MaxAttemptsDefault,
					Metadata:     []byte(`{"meta": "data"}`),
					Priority:     rivercommon.PriorityDefault,
					Queue:        rivercommon.QueueDefault,
					ScheduledAt:  &now,
					State:        rivertype.JobStateAvailable,
					Tags:         []string{"tag"},
					UniqueKey:    []byte("unique-key-no-returning-" + strconv.Itoa(i)),
					UniqueStates: 0xff,
				}
			}

			count, err := exec.JobInsertFastManyNoReturning(ctx, &riverdriver.JobInsertFastManyParams{
				Jobs:   insertParams,
				Schema: "",
			})
			require.NoError(t, err)
			require.Len(t, insertParams, count)

			jobsAfter, err := exec.JobGetByKindMany(ctx, &riverdriver.JobGetByKindManyParams{
				Kind:   []string{"test_kind"},
				Schema: "",
			})
			require.NoError(t, err)
			require.Len(t, jobsAfter, len(insertParams))
			for i, job := range jobsAfter {
				require.Equal(t, 0, job.Attempt)
				require.Nil(t, job.AttemptedAt)
				require.WithinDuration(t, now.Add(time.Duration(i)*5*time.Second), job.CreatedAt, time.Millisecond)
				require.JSONEq(t, `{"encoded": "args"}`, string(job.EncodedArgs))
				require.Empty(t, job.Errors)
				require.Nil(t, job.FinalizedAt)
				require.Equal(t, "test_kind", job.Kind)
				require.Equal(t, rivercommon.MaxAttemptsDefault, job.MaxAttempts)
				require.JSONEq(t, `{"meta": "data"}`, string(job.Metadata))
				require.Equal(t, rivercommon.PriorityDefault, job.Priority)
				require.Equal(t, rivercommon.QueueDefault, job.Queue)
				requireEqualTime(t, now, job.ScheduledAt)
				require.Equal(t, rivertype.JobStateAvailable, job.State)
				require.Equal(t, []string{"tag"}, job.Tags)
				require.Equal(t, []byte("unique-key-no-returning-"+strconv.Itoa(i)), job.UniqueKey)
			}
		})

		t.Run("MissingCreatedAtDefaultsToNow", func(t *testing.T) {
			t.Parallel()

			exec, _ := setup(ctx, t)

			insertParams := make([]*riverdriver.JobInsertFastParams, 10)
			for i := 0; i < len(insertParams); i++ {
				insertParams[i] = &riverdriver.JobInsertFastParams{
					CreatedAt:   nil, // explicit nil
					EncodedArgs: []byte(`{"encoded": "args"}`),
					Kind:        "test_kind",
					MaxAttempts: rivercommon.MaxAttemptsDefault,
					Metadata:    []byte(`{"meta": "data"}`),
					Priority:    rivercommon.PriorityDefault,
					Queue:       rivercommon.QueueDefault,
					ScheduledAt: ptrutil.Ptr(time.Now().UTC()),
					State:       rivertype.JobStateAvailable,
					Tags:        []string{"tag"},
				}
			}

			count, err := exec.JobInsertFastManyNoReturning(ctx, &riverdriver.JobInsertFastManyParams{
				Jobs:   insertParams,
				Schema: "",
			})
			require.NoError(t, err)
			require.Len(t, insertParams, count)

			jobsAfter, err := exec.JobGetByKindMany(ctx, &riverdriver.JobGetByKindManyParams{
				Kind:   []string{"test_kind"},
				Schema: "",
			})
			require.NoError(t, err)
			require.Len(t, jobsAfter, len(insertParams))
			for _, job := range jobsAfter {
				require.WithinDuration(t, time.Now().UTC(), job.CreatedAt, 2*time.Second)
			}
		})

		t.Run("MissingScheduledAtDefaultsToNow", func(t *testing.T) {
			t.Parallel()

			exec, _ := setup(ctx, t)

			insertParams := make([]*riverdriver.JobInsertFastParams, 10)
			for i := 0; i < len(insertParams); i++ {
				insertParams[i] = &riverdriver.JobInsertFastParams{
					EncodedArgs: []byte(`{"encoded": "args"}`),
					Kind:        "test_kind",
					MaxAttempts: rivercommon.MaxAttemptsDefault,
					Metadata:    []byte(`{"meta": "data"}`),
					Priority:    rivercommon.PriorityDefault,
					Queue:       rivercommon.QueueDefault,
					ScheduledAt: nil, // explicit nil
					State:       rivertype.JobStateAvailable,
					Tags:        []string{"tag"},
				}
			}

			count, err := exec.JobInsertFastManyNoReturning(ctx, &riverdriver.JobInsertFastManyParams{
				Jobs:   insertParams,
				Schema: "",
			})
			require.NoError(t, err)
			require.Len(t, insertParams, count)

			jobsAfter, err := exec.JobGetByKindMany(ctx, &riverdriver.JobGetByKindManyParams{
				Kind:   []string{"test_kind"},
				Schema: "",
			})
			require.NoError(t, err)
			require.Len(t, jobsAfter, len(insertParams))
			for _, job := range jobsAfter {
				require.WithinDuration(t, time.Now().UTC(), job.ScheduledAt, 2*time.Second)
			}
		})

		t.Run("AlternateSchema", func(t *testing.T) {
			t.Parallel()

			var (
				driver, schema = driverWithSchema(ctx, t)
				exec           = driver.GetExecutor()
			)

			// This test needs to use a time from before the transaction begins, otherwise
			// the newly-scheduled jobs won't yet show as available because their
			// scheduled_at (which gets a default value from time.Now() in code) will be
			// after the start of the transaction.
			now := time.Now().UTC().Add(-1 * time.Minute)

			insertParams := make([]*riverdriver.JobInsertFastParams, 10)
			for i := 0; i < len(insertParams); i++ {
				insertParams[i] = &riverdriver.JobInsertFastParams{
					CreatedAt:    ptrutil.Ptr(now.Add(time.Duration(i) * 5 * time.Second)),
					EncodedArgs:  []byte(`{"encoded": "args"}`),
					Kind:         "test_kind",
					MaxAttempts:  rivercommon.MaxAttemptsDefault,
					Metadata:     []byte(`{"meta": "data"}`),
					Priority:     rivercommon.PriorityDefault,
					Queue:        rivercommon.QueueDefault,
					ScheduledAt:  &now,
					State:        rivertype.JobStateAvailable,
					Tags:         []string{"tag"},
					UniqueKey:    []byte("unique-key-no-returning-" + strconv.Itoa(i)),
					UniqueStates: 0xff,
				}
			}

			count, err := exec.JobInsertFastManyNoReturning(ctx, &riverdriver.JobInsertFastManyParams{
				Jobs:   insertParams,
				Schema: schema,
			})
			require.NoError(t, err)
			require.Len(t, insertParams, count)

			jobsAfter, err := exec.JobGetByKindMany(ctx, &riverdriver.JobGetByKindManyParams{
				Kind:   []string{"test_kind"},
				Schema: schema,
			})
			require.NoError(t, err)
			require.Len(t, jobsAfter, len(insertParams))
		})
	})

	t.Run("JobInsertFull", func(t *testing.T) {
		t.Parallel()

		t.Run("MinimalArgsWithDefaults", func(t *testing.T) {
			t.Parallel()

			exec, _ := setup(ctx, t)

			job, err := exec.JobInsertFull(ctx, &riverdriver.JobInsertFullParams{
				EncodedArgs: []byte(`{"encoded": "args"}`),
				Kind:        "test_kind",
				MaxAttempts: rivercommon.MaxAttemptsDefault,
				Priority:    rivercommon.PriorityDefault,
				Queue:       rivercommon.QueueDefault,
				State:       rivertype.JobStateAvailable,
			})
			require.NoError(t, err)
			require.Equal(t, 0, job.Attempt)
			require.Nil(t, job.AttemptedAt)
			require.WithinDuration(t, time.Now().UTC(), job.CreatedAt, 2*time.Second)
			require.JSONEq(t, `{"encoded": "args"}`, string(job.EncodedArgs))
			require.Empty(t, job.Errors)
			require.Nil(t, job.FinalizedAt)
			require.Equal(t, "test_kind", job.Kind)
			require.Equal(t, rivercommon.MaxAttemptsDefault, job.MaxAttempts)
			require.Equal(t, rivercommon.QueueDefault, job.Queue)
			require.Equal(t, rivertype.JobStateAvailable, job.State)
		})

		t.Run("AllArgs", func(t *testing.T) {
			t.Parallel()

			exec, _ := setup(ctx, t)

			now := time.Now().UTC()

			job, err := exec.JobInsertFull(ctx, &riverdriver.JobInsertFullParams{
				Attempt:     3,
				AttemptedAt: &now,
				AttemptedBy: []string{"worker1", "worker2"},
				CreatedAt:   &now,
				EncodedArgs: []byte(`{"encoded": "args"}`),
				Errors:      [][]byte{[]byte(`{"error": "message"}`)},
				FinalizedAt: &now,
				Kind:        "test_kind",
				MaxAttempts: 6,
				Metadata:    []byte(`{"meta": "data"}`),
				Priority:    2,
				Queue:       "queue_name",
				ScheduledAt: &now,
				State:       rivertype.JobStateCompleted,
				Tags:        []string{"tag"},
				UniqueKey:   []byte("unique-key"),
			})
			require.NoError(t, err)
			require.Equal(t, 3, job.Attempt)
			requireEqualTime(t, now, *job.AttemptedAt)
			require.Equal(t, []string{"worker1", "worker2"}, job.AttemptedBy)
			requireEqualTime(t, now, job.CreatedAt)
			require.JSONEq(t, `{"encoded": "args"}`, string(job.EncodedArgs))
			require.Equal(t, "message", job.Errors[0].Error)
			requireEqualTime(t, now, *job.FinalizedAt)
			require.Equal(t, "test_kind", job.Kind)
			require.Equal(t, 6, job.MaxAttempts)
			require.JSONEq(t, `{"meta": "data"}`, string(job.Metadata))
			require.Equal(t, 2, job.Priority)
			require.Equal(t, "queue_name", job.Queue)
			requireEqualTime(t, now, job.ScheduledAt)
			require.Equal(t, rivertype.JobStateCompleted, job.State)
			require.Equal(t, []string{"tag"}, job.Tags)
			require.Equal(t, []byte("unique-key"), job.UniqueKey)
		})

		t.Run("JobFinalizedAtConstraint", func(t *testing.T) {
			t.Parallel()

			capitalizeJobState := func(state rivertype.JobState) string {
				return cases.Title(language.English, cases.NoLower).String(string(state))
			}

			for _, state := range []rivertype.JobState{
				rivertype.JobStateCancelled,
				rivertype.JobStateCompleted,
				rivertype.JobStateDiscarded,
			} {
				t.Run(fmt.Sprintf("CannotSetState%sWithoutFinalizedAt", capitalizeJobState(state)), func(t *testing.T) {
					t.Parallel()

					exec, _ := setup(ctx, t)
					// Create a job with the target state but without a finalized_at,
					// expect an error:
					params := testfactory.Job_Build(t, &testfactory.JobOpts{
						State: &state,
					})
					params.FinalizedAt = nil
					_, err := exec.JobInsertFull(ctx, params)
					require.ErrorContains(t, err, "violates check constraint \"finalized_or_finalized_at_null\"")
				})

				t.Run(fmt.Sprintf("CanSetState%sWithFinalizedAt", capitalizeJobState(state)), func(t *testing.T) {
					t.Parallel()

					exec, _ := setup(ctx, t)

					// Create a job with the target state but with a finalized_at, expect
					// no error:
					_, err := exec.JobInsertFull(ctx, testfactory.Job_Build(t, &testfactory.JobOpts{
						FinalizedAt: ptrutil.Ptr(time.Now()),
						State:       &state,
					}))
					require.NoError(t, err)
				})
			}

			for _, state := range []rivertype.JobState{
				rivertype.JobStateAvailable,
				rivertype.JobStateRetryable,
				rivertype.JobStateRunning,
				rivertype.JobStateScheduled,
			} {
				t.Run(fmt.Sprintf("CanSetState%sWithoutFinalizedAt", capitalizeJobState(state)), func(t *testing.T) {
					t.Parallel()

					exec, _ := setup(ctx, t)

					// Create a job with the target state but without a finalized_at,
					// expect no error:
					_, err := exec.JobInsertFull(ctx, testfactory.Job_Build(t, &testfactory.JobOpts{
						State: &state,
					}))
					require.NoError(t, err)
				})

				t.Run(fmt.Sprintf("CannotSetState%sWithFinalizedAt", capitalizeJobState(state)), func(t *testing.T) {
					t.Parallel()

					exec, _ := setup(ctx, t)

					// Create a job with the target state but with a finalized_at, expect
					// an error:
					_, err := exec.JobInsertFull(ctx, testfactory.Job_Build(t, &testfactory.JobOpts{
						FinalizedAt: ptrutil.Ptr(time.Now()),
						State:       &state,
					}))
					require.ErrorContains(t, err, "violates check constraint \"finalized_or_finalized_at_null\"")
				})
			}
		})
	})

	t.Run("JobList", func(t *testing.T) {
		t.Parallel()

		t.Run("ListsJobs", func(t *testing.T) {
			exec, _ := setup(ctx, t)

			now := time.Now().UTC()

			job := testfactory.Job(ctx, t, exec, &testfactory.JobOpts{
				Attempt:      ptrutil.Ptr(3),
				AttemptedAt:  &now,
				CreatedAt:    &now,
				EncodedArgs:  []byte(`{"encoded": "args"}`),
				Errors:       [][]byte{[]byte(`{"error": "message1"}`), []byte(`{"error": "message2"}`)},
				FinalizedAt:  &now,
				Metadata:     []byte(`{"meta": "data"}`),
				ScheduledAt:  &now,
				State:        ptrutil.Ptr(rivertype.JobStateCompleted),
				Tags:         []string{"tag"},
				UniqueKey:    []byte("unique-key"),
				UniqueStates: 0xFF,
			})

			// Does not match predicate (makes sure where clause is working).
			_ = testfactory.Job(ctx, t, exec, &testfactory.JobOpts{})

			fetchedJobs, err := exec.JobList(ctx, &riverdriver.JobListParams{
				Max:           100,
				NamedArgs:     map[string]any{"job_id_123": job.ID},
				OrderByClause: "id",
				WhereClause:   "id = @job_id_123",
			})
			require.NoError(t, err)
			require.Len(t, fetchedJobs, 1)

			fetchedJob := fetchedJobs[0]
			require.Equal(t, job.Attempt, fetchedJob.Attempt)
			require.Equal(t, job.AttemptedAt, fetchedJob.AttemptedAt)
			require.Equal(t, job.CreatedAt, fetchedJob.CreatedAt)
			require.Equal(t, job.EncodedArgs, fetchedJob.EncodedArgs)
			require.Equal(t, "message1", fetchedJob.Errors[0].Error)
			require.Equal(t, "message2", fetchedJob.Errors[1].Error)
			require.Equal(t, job.FinalizedAt, fetchedJob.FinalizedAt)
			require.Equal(t, job.Kind, fetchedJob.Kind)
			require.Equal(t, job.MaxAttempts, fetchedJob.MaxAttempts)
			require.Equal(t, job.Metadata, fetchedJob.Metadata)
			require.Equal(t, job.Priority, fetchedJob.Priority)
			require.Equal(t, job.Queue, fetchedJob.Queue)
			require.Equal(t, job.ScheduledAt, fetchedJob.ScheduledAt)
			require.Equal(t, job.State, fetchedJob.State)
			require.Equal(t, job.Tags, fetchedJob.Tags)
			require.Equal(t, []byte("unique-key"), fetchedJob.UniqueKey)
			require.Equal(t, rivertype.JobStates(), fetchedJob.UniqueStates)
		})

		t.Run("HandlesRequiredArgumentTypes", func(t *testing.T) {
			exec, _ := setup(ctx, t)

			job1 := testfactory.Job(ctx, t, exec, &testfactory.JobOpts{Kind: ptrutil.Ptr("test_kind1")})
			job2 := testfactory.Job(ctx, t, exec, &testfactory.JobOpts{Kind: ptrutil.Ptr("test_kind2")})

			{
				fetchedJobs, err := exec.JobList(ctx, &riverdriver.JobListParams{
					Max:           100,
					NamedArgs:     map[string]any{"kind": job1.Kind},
					OrderByClause: "id",
					WhereClause:   "kind = @kind",
				})
				require.NoError(t, err)
				require.Len(t, fetchedJobs, 1)
			}

			{
				fetchedJobs, err := exec.JobList(ctx, &riverdriver.JobListParams{
					Max:           100,
					NamedArgs:     map[string]any{"kind": []string{job1.Kind, job2.Kind}},
					OrderByClause: "id",
					WhereClause:   "kind = any(@kind::text[])",
				})
				require.NoError(t, err)
				require.Len(t, fetchedJobs, 2)
			}
		})
	})

	t.Run("JobRescueMany", func(t *testing.T) {
		t.Parallel()

		exec, _ := setup(ctx, t)

		now := time.Now().UTC()

		job1 := testfactory.Job(ctx, t, exec, &testfactory.JobOpts{State: ptrutil.Ptr(rivertype.JobStateRunning)})
		job2 := testfactory.Job(ctx, t, exec, &testfactory.JobOpts{State: ptrutil.Ptr(rivertype.JobStateRunning)})

		_, err := exec.JobRescueMany(ctx, &riverdriver.JobRescueManyParams{
			ID: []int64{
				job1.ID,
				job2.ID,
			},
			Error: [][]byte{
				[]byte(`{"error": "message1"}`),
				[]byte(`{"error": "message2"}`),
			},
			FinalizedAt: []time.Time{
				{},
				now,
			},
			ScheduledAt: []time.Time{
				now,
				now,
			},
			State: []string{
				string(rivertype.JobStateAvailable),
				string(rivertype.JobStateDiscarded),
			},
		})
		require.NoError(t, err)

		updatedJob1, err := exec.JobGetByID(ctx, &riverdriver.JobGetByIDParams{ID: job1.ID, Schema: ""})
		require.NoError(t, err)
		require.Equal(t, "message1", updatedJob1.Errors[0].Error)
		require.Nil(t, updatedJob1.FinalizedAt)
		requireEqualTime(t, now, updatedJob1.ScheduledAt)
		require.Equal(t, rivertype.JobStateAvailable, updatedJob1.State)

		updatedJob2, err := exec.JobGetByID(ctx, &riverdriver.JobGetByIDParams{ID: job2.ID, Schema: ""})
		require.NoError(t, err)
		require.Equal(t, "message2", updatedJob2.Errors[0].Error)
		requireEqualTime(t, now, *updatedJob2.FinalizedAt)
		requireEqualTime(t, now, updatedJob2.ScheduledAt)
		require.Equal(t, rivertype.JobStateDiscarded, updatedJob2.State)
	})

	t.Run("JobRetry", func(t *testing.T) {
		t.Parallel()

		t.Run("DoesNotUpdateARunningJob", func(t *testing.T) {
			t.Parallel()

			exec, _ := setup(ctx, t)

			job := testfactory.Job(ctx, t, exec, &testfactory.JobOpts{
				State: ptrutil.Ptr(rivertype.JobStateRunning),
			})

			jobAfter, err := exec.JobRetry(ctx, &riverdriver.JobRetryParams{
				ID:     job.ID,
				Schema: "",
			})
			require.NoError(t, err)
			require.Equal(t, rivertype.JobStateRunning, jobAfter.State)
			require.WithinDuration(t, job.ScheduledAt, jobAfter.ScheduledAt, time.Microsecond)

			jobUpdated, err := exec.JobGetByID(ctx, &riverdriver.JobGetByIDParams{ID: job.ID, Schema: ""})
			require.NoError(t, err)
			require.Equal(t, rivertype.JobStateRunning, jobUpdated.State)
		})

		for _, state := range []rivertype.JobState{
			rivertype.JobStateAvailable,
			rivertype.JobStateCancelled,
			rivertype.JobStateCompleted,
			rivertype.JobStateDiscarded,
			rivertype.JobStatePending,
			rivertype.JobStateRetryable,
			rivertype.JobStateScheduled,
		} {
			t.Run(fmt.Sprintf("UpdatesA_%s_JobToBeScheduledImmediately", state), func(t *testing.T) {
				t.Parallel()

				exec, _ := setup(ctx, t)

				now := time.Now().UTC()

				setFinalized := slices.Contains([]rivertype.JobState{
					rivertype.JobStateCancelled,
					rivertype.JobStateCompleted,
					rivertype.JobStateDiscarded,
				}, state)

				var finalizedAt *time.Time
				if setFinalized {
					finalizedAt = &now
				}

				job := testfactory.Job(ctx, t, exec, &testfactory.JobOpts{
					FinalizedAt: finalizedAt,
					ScheduledAt: ptrutil.Ptr(now.Add(1 * time.Hour)),
					State:       &state,
				})

				jobAfter, err := exec.JobRetry(ctx, &riverdriver.JobRetryParams{
					ID:     job.ID,
					Schema: "",
				})
				require.NoError(t, err)
				require.Equal(t, rivertype.JobStateAvailable, jobAfter.State)
				require.WithinDuration(t, time.Now().UTC(), jobAfter.ScheduledAt, 250*time.Millisecond) // TODO: Bad clock-based test

				jobUpdated, err := exec.JobGetByID(ctx, &riverdriver.JobGetByIDParams{ID: job.ID, Schema: ""})
				require.NoError(t, err)
				require.Equal(t, rivertype.JobStateAvailable, jobUpdated.State)
				require.Nil(t, jobUpdated.FinalizedAt)
			})
		}

		t.Run("AltersScheduledAtForAlreadyCompletedJob", func(t *testing.T) {
			// A job which has already completed will have a ScheduledAt that could be
			// long in the past. Now that we're re-scheduling it, we should update that
			// to the current time to slot it in alongside other recently-scheduled jobs
			// and not skip the line; also, its wait duration can't be calculated
			// accurately if we don't reset the scheduled_at.
			t.Parallel()

			exec, _ := setup(ctx, t)

			now := time.Now().UTC()

			job := testfactory.Job(ctx, t, exec, &testfactory.JobOpts{
				FinalizedAt: &now,
				ScheduledAt: ptrutil.Ptr(now.Add(-1 * time.Hour)),
				State:       ptrutil.Ptr(rivertype.JobStateCompleted),
			})

			jobAfter, err := exec.JobRetry(ctx, &riverdriver.JobRetryParams{
				ID:     job.ID,
				Schema: "",
			})
			require.NoError(t, err)
			require.Equal(t, rivertype.JobStateAvailable, jobAfter.State)
			require.WithinDuration(t, now, jobAfter.ScheduledAt, 5*time.Second)
		})

		t.Run("DoesNotAlterScheduledAtIfInThePastAndJobAlreadyAvailable", func(t *testing.T) {
			// We don't want to update ScheduledAt if the job was already available
			// because doing so can make it lose its place in line.
			t.Parallel()

			exec, _ := setup(ctx, t)

			now := time.Now().UTC()

			job := testfactory.Job(ctx, t, exec, &testfactory.JobOpts{
				ScheduledAt: ptrutil.Ptr(now.Add(-1 * time.Hour)),
			})

			jobAfter, err := exec.JobRetry(ctx, &riverdriver.JobRetryParams{
				ID:     job.ID,
				Schema: "",
			})
			require.NoError(t, err)
			require.Equal(t, rivertype.JobStateAvailable, jobAfter.State)
			require.WithinDuration(t, job.ScheduledAt, jobAfter.ScheduledAt, time.Microsecond)

			jobUpdated, err := exec.JobGetByID(ctx, &riverdriver.JobGetByIDParams{ID: job.ID, Schema: ""})
			require.NoError(t, err)
			require.Equal(t, rivertype.JobStateAvailable, jobUpdated.State)
		})

		t.Run("ReturnsErrNotFoundIfJobNotFound", func(t *testing.T) {
			t.Parallel()

			exec, _ := setup(ctx, t)

			_, err := exec.JobRetry(ctx, &riverdriver.JobRetryParams{
				ID:     0,
				Schema: "",
			})
			require.Error(t, err)
			require.ErrorIs(t, err, rivertype.ErrNotFound)
		})
	})

	t.Run("JobSchedule", func(t *testing.T) {
		t.Parallel()

		t.Run("BasicScheduling", func(t *testing.T) {
			exec, _ := setup(ctx, t)

			var (
				horizon       = time.Now()
				beforeHorizon = horizon.Add(-1 * time.Minute)
				afterHorizon  = horizon.Add(1 * time.Minute)
			)

			job1 := testfactory.Job(ctx, t, exec, &testfactory.JobOpts{ScheduledAt: &beforeHorizon, State: ptrutil.Ptr(rivertype.JobStateRetryable)})
			job2 := testfactory.Job(ctx, t, exec, &testfactory.JobOpts{ScheduledAt: &beforeHorizon, State: ptrutil.Ptr(rivertype.JobStateScheduled)})
			job3 := testfactory.Job(ctx, t, exec, &testfactory.JobOpts{ScheduledAt: &beforeHorizon, State: ptrutil.Ptr(rivertype.JobStateScheduled)})

			// States that aren't scheduled.
			_ = testfactory.Job(ctx, t, exec, &testfactory.JobOpts{ScheduledAt: &beforeHorizon, State: ptrutil.Ptr(rivertype.JobStateAvailable)})
			_ = testfactory.Job(ctx, t, exec, &testfactory.JobOpts{FinalizedAt: &beforeHorizon, ScheduledAt: &beforeHorizon, State: ptrutil.Ptr(rivertype.JobStateCompleted)})
			_ = testfactory.Job(ctx, t, exec, &testfactory.JobOpts{FinalizedAt: &beforeHorizon, ScheduledAt: &beforeHorizon, State: ptrutil.Ptr(rivertype.JobStateDiscarded)})

			// Right state, but after horizon.
			_ = testfactory.Job(ctx, t, exec, &testfactory.JobOpts{ScheduledAt: &afterHorizon, State: ptrutil.Ptr(rivertype.JobStateRetryable)})
			_ = testfactory.Job(ctx, t, exec, &testfactory.JobOpts{ScheduledAt: &afterHorizon, State: ptrutil.Ptr(rivertype.JobStateScheduled)})

			// First two scheduled because of limit.
			result, err := exec.JobSchedule(ctx, &riverdriver.JobScheduleParams{
				Max: 2,
				Now: horizon,
			})
			require.NoError(t, err)
			require.Len(t, result, 2)

			// And then job3 scheduled.
			result, err = exec.JobSchedule(ctx, &riverdriver.JobScheduleParams{
				Max: 2,
				Now: horizon,
			})
			require.NoError(t, err)
			require.Len(t, result, 1)

			updatedJob1, err := exec.JobGetByID(ctx, &riverdriver.JobGetByIDParams{ID: job1.ID, Schema: ""})
			require.NoError(t, err)
			require.Equal(t, rivertype.JobStateAvailable, updatedJob1.State)

			updatedJob2, err := exec.JobGetByID(ctx, &riverdriver.JobGetByIDParams{ID: job2.ID, Schema: ""})
			require.NoError(t, err)
			require.Equal(t, rivertype.JobStateAvailable, updatedJob2.State)

			updatedJob3, err := exec.JobGetByID(ctx, &riverdriver.JobGetByIDParams{ID: job3.ID, Schema: ""})
			require.NoError(t, err)
			require.Equal(t, rivertype.JobStateAvailable, updatedJob3.State)
		})

		t.Run("HandlesUniqueConflicts", func(t *testing.T) {
			t.Parallel()

			exec, _ := setup(ctx, t)

			var (
				horizon       = time.Now()
				beforeHorizon = horizon.Add(-1 * time.Minute)
			)

			defaultUniqueStates := []rivertype.JobState{
				rivertype.JobStateAvailable,
				rivertype.JobStatePending,
				rivertype.JobStateRetryable,
				rivertype.JobStateRunning,
				rivertype.JobStateScheduled,
			}
			// The default unique state list, minus retryable to allow for these conflicts:
			nonRetryableUniqueStates := []rivertype.JobState{
				rivertype.JobStateAvailable,
				rivertype.JobStatePending,
				rivertype.JobStateRunning,
				rivertype.JobStateScheduled,
			}

			job1 := testfactory.Job(ctx, t, exec, &testfactory.JobOpts{
				ScheduledAt:  &beforeHorizon,
				State:        ptrutil.Ptr(rivertype.JobStateRetryable),
				UniqueKey:    []byte("unique-key-1"),
				UniqueStates: dbunique.UniqueStatesToBitmask(nonRetryableUniqueStates),
			})
			job2 := testfactory.Job(ctx, t, exec, &testfactory.JobOpts{
				ScheduledAt:  &beforeHorizon,
				State:        ptrutil.Ptr(rivertype.JobStateRetryable),
				UniqueKey:    []byte("unique-key-2"),
				UniqueStates: dbunique.UniqueStatesToBitmask(nonRetryableUniqueStates),
			})
			// job3 has no conflict (it's the only one with this key), so it should be
			// scheduled.
			job3 := testfactory.Job(ctx, t, exec, &testfactory.JobOpts{
				ScheduledAt:  &beforeHorizon,
				State:        ptrutil.Ptr(rivertype.JobStateRetryable),
				UniqueKey:    []byte("unique-key-3"),
				UniqueStates: dbunique.UniqueStatesToBitmask(defaultUniqueStates),
			})

			// This one is a conflict with job1 because it's already running and has
			// the same unique properties:
			_ = testfactory.Job(ctx, t, exec, &testfactory.JobOpts{
				ScheduledAt:  &beforeHorizon,
				State:        ptrutil.Ptr(rivertype.JobStateRunning),
				UniqueKey:    []byte("unique-key-1"),
				UniqueStates: dbunique.UniqueStatesToBitmask(nonRetryableUniqueStates),
			})
			// This one is *not* a conflict with job2 because it's completed, which
			// isn't in the unique states:
			_ = testfactory.Job(ctx, t, exec, &testfactory.JobOpts{
				ScheduledAt:  &beforeHorizon,
				State:        ptrutil.Ptr(rivertype.JobStateCompleted),
				UniqueKey:    []byte("unique-key-2"),
				UniqueStates: dbunique.UniqueStatesToBitmask(nonRetryableUniqueStates),
			})

			result, err := exec.JobSchedule(ctx, &riverdriver.JobScheduleParams{
				Max: 100,
				Now: horizon,
			})
			require.NoError(t, err)
			require.Len(t, result, 3)

			updatedJob1, err := exec.JobGetByID(ctx, &riverdriver.JobGetByIDParams{ID: job1.ID, Schema: ""})
			require.NoError(t, err)
			require.Equal(t, rivertype.JobStateDiscarded, updatedJob1.State)
			require.Equal(t, "scheduler_discarded", gjson.GetBytes(updatedJob1.Metadata, "unique_key_conflict").String())

			updatedJob2, err := exec.JobGetByID(ctx, &riverdriver.JobGetByIDParams{ID: job2.ID, Schema: ""})
			require.NoError(t, err)
			require.Equal(t, rivertype.JobStateAvailable, updatedJob2.State)
			require.False(t, gjson.GetBytes(updatedJob2.Metadata, "unique_key_conflict").Exists())

			updatedJob3, err := exec.JobGetByID(ctx, &riverdriver.JobGetByIDParams{ID: job3.ID, Schema: ""})
			require.NoError(t, err)
			require.Equal(t, rivertype.JobStateAvailable, updatedJob3.State)
			require.False(t, gjson.GetBytes(updatedJob3.Metadata, "unique_key_conflict").Exists())
		})

		t.Run("SchedulingTwoRetryableJobsThatWillConflictWithEachOther", func(t *testing.T) {
			t.Parallel()

			exec, _ := setup(ctx, t)

			var (
				horizon       = time.Now()
				beforeHorizon = horizon.Add(-1 * time.Minute)
			)

			// The default unique state list, minus retryable to allow for these conflicts:
			nonRetryableUniqueStates := []rivertype.JobState{
				rivertype.JobStateAvailable,
				rivertype.JobStatePending,
				rivertype.JobStateRunning,
				rivertype.JobStateScheduled,
			}

			job1 := testfactory.Job(ctx, t, exec, &testfactory.JobOpts{
				ScheduledAt:  &beforeHorizon,
				State:        ptrutil.Ptr(rivertype.JobStateRetryable),
				UniqueKey:    []byte("unique-key-1"),
				UniqueStates: dbunique.UniqueStatesToBitmask(nonRetryableUniqueStates),
			})
			job2 := testfactory.Job(ctx, t, exec, &testfactory.JobOpts{
				ScheduledAt:  &beforeHorizon,
				State:        ptrutil.Ptr(rivertype.JobStateRetryable),
				UniqueKey:    []byte("unique-key-1"),
				UniqueStates: dbunique.UniqueStatesToBitmask(nonRetryableUniqueStates),
			})

			result, err := exec.JobSchedule(ctx, &riverdriver.JobScheduleParams{
				Max: 100,
				Now: horizon,
			})
			require.NoError(t, err)
			require.Len(t, result, 2)

			updatedJob1, err := exec.JobGetByID(ctx, &riverdriver.JobGetByIDParams{ID: job1.ID, Schema: ""})
			require.NoError(t, err)
			require.Equal(t, rivertype.JobStateAvailable, updatedJob1.State)
			require.False(t, gjson.GetBytes(updatedJob1.Metadata, "unique_key_conflict").Exists())

			updatedJob2, err := exec.JobGetByID(ctx, &riverdriver.JobGetByIDParams{ID: job2.ID, Schema: ""})
			require.NoError(t, err)
			require.Equal(t, rivertype.JobStateDiscarded, updatedJob2.State)
			require.Equal(t, "scheduler_discarded", gjson.GetBytes(updatedJob2.Metadata, "unique_key_conflict").String())
		})
	})

	makeErrPayload := func(t *testing.T, now time.Time) []byte {
		t.Helper()

		errPayload, err := json.Marshal(rivertype.AttemptError{
			Attempt: 1, At: now, Error: "fake error", Trace: "foo.go:123\nbar.go:456",
		})
		require.NoError(t, err)
		return errPayload
	}

	setStateManyParams := func(params ...*riverdriver.JobSetStateIfRunningParams) *riverdriver.JobSetStateIfRunningManyParams {
		batchParams := &riverdriver.JobSetStateIfRunningManyParams{}
		for _, param := range params {
			var (
				attempt     *int
				errData     []byte
				finalizedAt *time.Time
				scheduledAt *time.Time
			)
			if param.Attempt != nil {
				attempt = param.Attempt
			}
			if param.ErrData != nil {
				errData = param.ErrData
			}
			if param.FinalizedAt != nil {
				finalizedAt = param.FinalizedAt
			}
			if param.ScheduledAt != nil {
				scheduledAt = param.ScheduledAt
			}

			batchParams.ID = append(batchParams.ID, param.ID)
			batchParams.Attempt = append(batchParams.Attempt, attempt)
			batchParams.ErrData = append(batchParams.ErrData, errData)
			batchParams.FinalizedAt = append(batchParams.FinalizedAt, finalizedAt)
			batchParams.MetadataDoMerge = append(batchParams.MetadataDoMerge, param.MetadataDoMerge)
			batchParams.MetadataUpdates = append(batchParams.MetadataUpdates, param.MetadataUpdates)
			batchParams.ScheduledAt = append(batchParams.ScheduledAt, scheduledAt)
			batchParams.State = append(batchParams.State, param.State)
		}

		return batchParams
	}

	t.Run("JobSetStateIfRunningMany_JobSetStateCompleted", func(t *testing.T) {
		t.Parallel()

		t.Run("CompletesARunningJob", func(t *testing.T) {
			t.Parallel()

			exec, _ := setup(ctx, t)

			now := time.Now().UTC()

			job := testfactory.Job(ctx, t, exec, &testfactory.JobOpts{
				State:     ptrutil.Ptr(rivertype.JobStateRunning),
				UniqueKey: []byte("unique-key"),
			})

			jobsAfter, err := exec.JobSetStateIfRunningMany(ctx, setStateManyParams(riverdriver.JobSetStateCompleted(job.ID, now, nil)))
			require.NoError(t, err)
			jobAfter := jobsAfter[0]
			require.Equal(t, rivertype.JobStateCompleted, jobAfter.State)
			require.WithinDuration(t, now, *jobAfter.FinalizedAt, time.Microsecond)

			jobUpdated, err := exec.JobGetByID(ctx, &riverdriver.JobGetByIDParams{ID: job.ID, Schema: ""})
			require.NoError(t, err)
			require.Equal(t, rivertype.JobStateCompleted, jobUpdated.State)
			require.Equal(t, "unique-key", string(jobUpdated.UniqueKey))
		})

		t.Run("DoesNotCompleteARetryableJob", func(t *testing.T) {
			t.Parallel()

			exec, _ := setup(ctx, t)

			now := time.Now().UTC()

			job := testfactory.Job(ctx, t, exec, &testfactory.JobOpts{
				State:     ptrutil.Ptr(rivertype.JobStateRetryable),
				UniqueKey: []byte("unique-key"),
			})

			jobsAfter, err := exec.JobSetStateIfRunningMany(ctx, setStateManyParams(riverdriver.JobSetStateCompleted(job.ID, now, nil)))
			require.NoError(t, err)
			jobAfter := jobsAfter[0]
			require.Equal(t, rivertype.JobStateRetryable, jobAfter.State)
			require.Nil(t, jobAfter.FinalizedAt)

			jobUpdated, err := exec.JobGetByID(ctx, &riverdriver.JobGetByIDParams{ID: job.ID, Schema: ""})
			require.NoError(t, err)
			require.Equal(t, rivertype.JobStateRetryable, jobUpdated.State)
			require.Equal(t, "unique-key", string(jobUpdated.UniqueKey))
		})

		t.Run("StoresMetadataUpdates", func(t *testing.T) {
			t.Parallel()

			exec, _ := setup(ctx, t)

			now := time.Now().UTC()

			job := testfactory.Job(ctx, t, exec, &testfactory.JobOpts{
				Metadata:  []byte(`{"foo":"baz", "something":"else"}`),
				State:     ptrutil.Ptr(rivertype.JobStateRunning),
				UniqueKey: []byte("unique-key"),
			})

			jobsAfter, err := exec.JobSetStateIfRunningMany(ctx, setStateManyParams(riverdriver.JobSetStateCompleted(job.ID, now, []byte(`{"a":"b", "foo":"bar"}`))))
			require.NoError(t, err)
			jobAfter := jobsAfter[0]
			require.Equal(t, rivertype.JobStateCompleted, jobAfter.State)
			require.JSONEq(t, `{"a":"b", "foo":"bar", "something":"else"}`, string(jobAfter.Metadata))
		})
	})

	t.Run("JobSetStateIfRunningMany_JobSetStateErrored", func(t *testing.T) {
		t.Parallel()

		t.Run("SetsARunningJobToRetryable", func(t *testing.T) {
			t.Parallel()

			exec, _ := setup(ctx, t)

			now := time.Now().UTC()

			job := testfactory.Job(ctx, t, exec, &testfactory.JobOpts{
				State:     ptrutil.Ptr(rivertype.JobStateRunning),
				UniqueKey: []byte("unique-key"),
			})

			jobsAfter, err := exec.JobSetStateIfRunningMany(ctx, setStateManyParams(riverdriver.JobSetStateErrorRetryable(job.ID, now, makeErrPayload(t, now), nil)))
			require.NoError(t, err)
			jobAfter := jobsAfter[0]
			require.Equal(t, rivertype.JobStateRetryable, jobAfter.State)
			require.WithinDuration(t, now, jobAfter.ScheduledAt, time.Microsecond)

			jobUpdated, err := exec.JobGetByID(ctx, &riverdriver.JobGetByIDParams{ID: job.ID, Schema: ""})
			require.NoError(t, err)
			require.Equal(t, rivertype.JobStateRetryable, jobUpdated.State)
			require.Equal(t, "unique-key", string(jobUpdated.UniqueKey))

			// validate error payload:
			require.Len(t, jobAfter.Errors, 1)
			require.Equal(t, now, jobAfter.Errors[0].At)
			require.Equal(t, 1, jobAfter.Errors[0].Attempt)
			require.Equal(t, "fake error", jobAfter.Errors[0].Error)
			require.Equal(t, "foo.go:123\nbar.go:456", jobAfter.Errors[0].Trace)
		})

		t.Run("DoesNotTouchAlreadyRetryableJobWithNoMetadataUpdates", func(t *testing.T) {
			t.Parallel()

			exec, _ := setup(ctx, t)

			now := time.Now().UTC()

			job := testfactory.Job(ctx, t, exec, &testfactory.JobOpts{
				State:       ptrutil.Ptr(rivertype.JobStateRetryable),
				ScheduledAt: ptrutil.Ptr(now.Add(10 * time.Second)),
			})

			jobsAfter, err := exec.JobSetStateIfRunningMany(ctx, setStateManyParams(riverdriver.JobSetStateErrorRetryable(job.ID, now, makeErrPayload(t, now), nil)))
			require.NoError(t, err)
			jobAfter := jobsAfter[0]
			require.Equal(t, rivertype.JobStateRetryable, jobAfter.State)
			require.WithinDuration(t, job.ScheduledAt, jobAfter.ScheduledAt, time.Microsecond)

			jobUpdated, err := exec.JobGetByID(ctx, &riverdriver.JobGetByIDParams{ID: job.ID, Schema: ""})
			require.NoError(t, err)
			require.Equal(t, rivertype.JobStateRetryable, jobUpdated.State)
			require.WithinDuration(t, job.ScheduledAt, jobAfter.ScheduledAt, time.Microsecond)
		})

		t.Run("UpdatesOnlyMetadataForAlreadyRetryableJobs", func(t *testing.T) {
			t.Parallel()

			exec, _ := setup(ctx, t)

			now := time.Now().UTC()

			job1 := testfactory.Job(ctx, t, exec, &testfactory.JobOpts{
				Metadata:    []byte(`{"baz":"qux", "foo":"bar"}`),
				State:       ptrutil.Ptr(rivertype.JobStateRetryable),
				ScheduledAt: ptrutil.Ptr(now.Add(10 * time.Second)),
			})

			jobsAfter, err := exec.JobSetStateIfRunningMany(ctx, setStateManyParams(
				riverdriver.JobSetStateErrorRetryable(job1.ID, now, makeErrPayload(t, now), []byte(`{"foo":"1", "output":{"a":"b"}}`)),
			))
			require.NoError(t, err)
			jobAfter := jobsAfter[0]
			require.Equal(t, rivertype.JobStateRetryable, jobAfter.State)
			require.JSONEq(t, `{"baz":"qux", "foo":"1", "output":{"a":"b"}}`, string(jobAfter.Metadata))
			require.Empty(t, jobAfter.Errors)
			require.Equal(t, job1.ScheduledAt, jobAfter.ScheduledAt)

			jobUpdated, err := exec.JobGetByID(ctx, &riverdriver.JobGetByIDParams{ID: job1.ID, Schema: ""})
			require.NoError(t, err)
			require.Equal(t, rivertype.JobStateRetryable, jobUpdated.State)
			require.JSONEq(t, `{"baz":"qux", "foo":"1", "output":{"a":"b"}}`, string(jobUpdated.Metadata))
			require.Empty(t, jobUpdated.Errors)
			require.Equal(t, job1.ScheduledAt, jobUpdated.ScheduledAt)
		})

		t.Run("SetsAJobWithCancelAttemptedAtToCancelled", func(t *testing.T) {
			// If a job has cancel_attempted_at in its metadata, it means that the user
			// tried to cancel the job with the Cancel API but that the job
			// finished/errored before the producer received the cancel notification.
			//
			// In this case, we want to move the job to cancelled instead of retryable
			// so that the job is not retried.
			t.Parallel()

			exec, _ := setup(ctx, t)

			now := time.Now().UTC()

			job := testfactory.Job(ctx, t, exec, &testfactory.JobOpts{
				Metadata:    []byte(fmt.Sprintf(`{"cancel_attempted_at":"%s"}`, time.Now().UTC().Format(time.RFC3339))),
				State:       ptrutil.Ptr(rivertype.JobStateRunning),
				ScheduledAt: ptrutil.Ptr(now.Add(-10 * time.Second)),
			})

			jobsAfter, err := exec.JobSetStateIfRunningMany(ctx, setStateManyParams(riverdriver.JobSetStateErrorRetryable(job.ID, now, makeErrPayload(t, now), nil)))
			require.NoError(t, err)
			jobAfter := jobsAfter[0]
			require.Equal(t, rivertype.JobStateCancelled, jobAfter.State)
			require.NotNil(t, jobAfter.FinalizedAt)
			// Loose assertion against FinalizedAt just to make sure it was set (it uses
			// the database's now() instead of a passed-in time):
			require.WithinDuration(t, time.Now().UTC(), *jobAfter.FinalizedAt, 2*time.Second)
			// ScheduledAt should not be touched:
			require.WithinDuration(t, job.ScheduledAt, jobAfter.ScheduledAt, time.Microsecond)

			// Errors should still be appended to:
			require.Len(t, jobAfter.Errors, 1)
			require.Contains(t, jobAfter.Errors[0].Error, "fake error")

			jobUpdated, err := exec.JobGetByID(ctx, &riverdriver.JobGetByIDParams{ID: job.ID, Schema: ""})
			require.NoError(t, err)
			require.Equal(t, rivertype.JobStateCancelled, jobUpdated.State)
			require.WithinDuration(t, job.ScheduledAt, jobAfter.ScheduledAt, time.Microsecond)
		})
	})

	t.Run("JobSetStateIfRunningMany_JobSetStateCancelled", func(t *testing.T) {
		t.Parallel()

		t.Run("CancelsARunningJob", func(t *testing.T) {
			t.Parallel()

			exec, _ := setup(ctx, t)

			now := time.Now().UTC()

			job := testfactory.Job(ctx, t, exec, &testfactory.JobOpts{
				State:        ptrutil.Ptr(rivertype.JobStateRunning),
				UniqueKey:    []byte("unique-key"),
				UniqueStates: 0xFF,
			})

			jobsAfter, err := exec.JobSetStateIfRunningMany(ctx, setStateManyParams(riverdriver.JobSetStateCancelled(job.ID, now, makeErrPayload(t, now), nil)))
			require.NoError(t, err)
			jobAfter := jobsAfter[0]
			require.Equal(t, rivertype.JobStateCancelled, jobAfter.State)
			require.WithinDuration(t, now, *jobAfter.FinalizedAt, time.Microsecond)

			jobUpdated, err := exec.JobGetByID(ctx, &riverdriver.JobGetByIDParams{ID: job.ID, Schema: ""})
			require.NoError(t, err)
			require.Equal(t, rivertype.JobStateCancelled, jobUpdated.State)
			require.Equal(t, "unique-key", string(jobUpdated.UniqueKey))
		})
	})

	t.Run("JobSetStateIfRunningMany_JobSetStateDiscarded", func(t *testing.T) {
		t.Parallel()

		t.Run("DiscardsARunningJob", func(t *testing.T) {
			t.Parallel()

			exec, _ := setup(ctx, t)

			now := time.Now().UTC()

			job := testfactory.Job(ctx, t, exec, &testfactory.JobOpts{
				State:        ptrutil.Ptr(rivertype.JobStateRunning),
				UniqueKey:    []byte("unique-key"),
				UniqueStates: 0xFF,
			})

			jobsAfter, err := exec.JobSetStateIfRunningMany(ctx, setStateManyParams(riverdriver.JobSetStateDiscarded(job.ID, now, makeErrPayload(t, now), nil)))
			require.NoError(t, err)
			jobAfter := jobsAfter[0]
			require.Equal(t, rivertype.JobStateDiscarded, jobAfter.State)
			require.WithinDuration(t, now, *jobAfter.FinalizedAt, time.Microsecond)
			require.Equal(t, "unique-key", string(jobAfter.UniqueKey))
			require.Equal(t, rivertype.JobStates(), jobAfter.UniqueStates)

			jobUpdated, err := exec.JobGetByID(ctx, &riverdriver.JobGetByIDParams{ID: job.ID, Schema: ""})
			require.NoError(t, err)
			require.Equal(t, rivertype.JobStateDiscarded, jobUpdated.State)
		})
	})

	t.Run("JobSetStateIfRunningMany_JobSetStateSnoozed", func(t *testing.T) {
		t.Parallel()

		t.Run("SnoozesARunningJob_WithNoPreexistingMetadata", func(t *testing.T) {
			t.Parallel()

			exec, _ := setup(ctx, t)

			now := time.Now().UTC()
			snoozeUntil := now.Add(1 * time.Minute)

			job := testfactory.Job(ctx, t, exec, &testfactory.JobOpts{
				Attempt:   ptrutil.Ptr(5),
				State:     ptrutil.Ptr(rivertype.JobStateRunning),
				UniqueKey: []byte("unique-key"),
			})

			jobsAfter, err := exec.JobSetStateIfRunningMany(ctx, setStateManyParams(riverdriver.JobSetStateSnoozed(job.ID, snoozeUntil, 4, []byte(`{"snoozes": 1}`))))
			require.NoError(t, err)
			jobAfter := jobsAfter[0]
			require.Equal(t, 4, jobAfter.Attempt)
			require.Equal(t, job.MaxAttempts, jobAfter.MaxAttempts)
			require.JSONEq(t, `{"snoozes": 1}`, string(jobAfter.Metadata))
			require.Equal(t, rivertype.JobStateScheduled, jobAfter.State)
			require.WithinDuration(t, snoozeUntil, jobAfter.ScheduledAt, time.Microsecond)

			jobUpdated, err := exec.JobGetByID(ctx, &riverdriver.JobGetByIDParams{ID: job.ID, Schema: ""})
			require.NoError(t, err)
			require.Equal(t, 4, jobUpdated.Attempt)
			require.Equal(t, job.MaxAttempts, jobUpdated.MaxAttempts)
			require.JSONEq(t, `{"snoozes": 1}`, string(jobUpdated.Metadata))
			require.Equal(t, rivertype.JobStateScheduled, jobUpdated.State)
			require.Equal(t, "unique-key", string(jobUpdated.UniqueKey))
		})

		t.Run("SnoozesARunningJob_WithPreexistingMetadata", func(t *testing.T) {
			t.Parallel()

			exec, _ := setup(ctx, t)

			now := time.Now().UTC()
			snoozeUntil := now.Add(1 * time.Minute)

			job := testfactory.Job(ctx, t, exec, &testfactory.JobOpts{
				Attempt:   ptrutil.Ptr(5),
				State:     ptrutil.Ptr(rivertype.JobStateRunning),
				UniqueKey: []byte("unique-key"),
				Metadata:  []byte(`{"foo": "bar", "snoozes": 5}`),
			})

			jobsAfter, err := exec.JobSetStateIfRunningMany(ctx, setStateManyParams(riverdriver.JobSetStateSnoozed(job.ID, snoozeUntil, 4, []byte(`{"snoozes": 6}`))))
			require.NoError(t, err)
			jobAfter := jobsAfter[0]
			require.Equal(t, 4, jobAfter.Attempt)
			require.Equal(t, job.MaxAttempts, jobAfter.MaxAttempts)
			require.JSONEq(t, `{"foo": "bar", "snoozes": 6}`, string(jobAfter.Metadata))
			require.Equal(t, rivertype.JobStateScheduled, jobAfter.State)
			require.WithinDuration(t, snoozeUntil, jobAfter.ScheduledAt, time.Microsecond)

			jobUpdated, err := exec.JobGetByID(ctx, &riverdriver.JobGetByIDParams{ID: job.ID, Schema: ""})
			require.NoError(t, err)
			require.Equal(t, 4, jobUpdated.Attempt)
			require.Equal(t, job.MaxAttempts, jobUpdated.MaxAttempts)
			require.JSONEq(t, `{"foo": "bar", "snoozes": 6}`, string(jobUpdated.Metadata))
			require.Equal(t, rivertype.JobStateScheduled, jobUpdated.State)
			require.Equal(t, "unique-key", string(jobUpdated.UniqueKey))
		})
	})

	t.Run("JobSetStateIfRunningMany_MultipleJobsAtOnce", func(t *testing.T) {
		t.Parallel()

		exec, _ := setup(ctx, t)

		now := time.Now().UTC()
		future := now.Add(10 * time.Second)

		job1 := testfactory.Job(ctx, t, exec, &testfactory.JobOpts{State: ptrutil.Ptr(rivertype.JobStateRunning)})
		job2 := testfactory.Job(ctx, t, exec, &testfactory.JobOpts{State: ptrutil.Ptr(rivertype.JobStateRunning)})
		job3 := testfactory.Job(ctx, t, exec, &testfactory.JobOpts{State: ptrutil.Ptr(rivertype.JobStateRunning)})

		jobsAfter, err := exec.JobSetStateIfRunningMany(ctx, setStateManyParams(
			riverdriver.JobSetStateCompleted(job1.ID, now, []byte(`{"a":"b"}`)),
			riverdriver.JobSetStateErrorRetryable(job2.ID, future, makeErrPayload(t, now), nil),
			riverdriver.JobSetStateCancelled(job3.ID, now, makeErrPayload(t, now), nil),
		))
		require.NoError(t, err)
		completedJob := jobsAfter[0]
		require.Equal(t, rivertype.JobStateCompleted, completedJob.State)
		require.WithinDuration(t, now, *completedJob.FinalizedAt, time.Microsecond)
		require.JSONEq(t, `{"a":"b"}`, string(completedJob.Metadata))

		retryableJob := jobsAfter[1]
		require.Equal(t, rivertype.JobStateRetryable, retryableJob.State)
		require.WithinDuration(t, future, retryableJob.ScheduledAt, time.Microsecond)
		// validate error payload:
		require.Len(t, retryableJob.Errors, 1)
		require.Equal(t, now, retryableJob.Errors[0].At)
		require.Equal(t, 1, retryableJob.Errors[0].Attempt)
		require.Equal(t, "fake error", retryableJob.Errors[0].Error)
		require.Equal(t, "foo.go:123\nbar.go:456", retryableJob.Errors[0].Trace)

		cancelledJob := jobsAfter[2]
		require.Equal(t, rivertype.JobStateCancelled, cancelledJob.State)
		require.WithinDuration(t, now, *cancelledJob.FinalizedAt, time.Microsecond)
	})

	t.Run("JobUpdate", func(t *testing.T) {
		t.Parallel()

		exec, _ := setup(ctx, t)

		job := testfactory.Job(ctx, t, exec, &testfactory.JobOpts{})

		now := time.Now().UTC()

		updatedJob, err := exec.JobUpdate(ctx, &riverdriver.JobUpdateParams{
			ID:                  job.ID,
			AttemptDoUpdate:     true,
			Attempt:             7,
			AttemptedAtDoUpdate: true,
			AttemptedAt:         &now,
			AttemptedByDoUpdate: true,
			AttemptedBy:         []string{"worker1"},
			ErrorsDoUpdate:      true,
			Errors:              [][]byte{[]byte(`{"error":"message"}`)},
			FinalizedAtDoUpdate: true,
			FinalizedAt:         &now,
			StateDoUpdate:       true,
			State:               rivertype.JobStateDiscarded,
		})
		require.NoError(t, err)
		require.Equal(t, 7, updatedJob.Attempt)
		requireEqualTime(t, now, *updatedJob.AttemptedAt)
		require.Equal(t, []string{"worker1"}, updatedJob.AttemptedBy)
		require.Equal(t, "message", updatedJob.Errors[0].Error)
		requireEqualTime(t, now, *updatedJob.FinalizedAt)
		require.Equal(t, rivertype.JobStateDiscarded, updatedJob.State)
	})

	const leaderTTL = 10 * time.Second

	t.Run("LeaderAttemptElect", func(t *testing.T) {
		t.Parallel()

		t.Run("ElectsLeader", func(t *testing.T) {
			t.Parallel()

			exec, _ := setup(ctx, t)

			now := time.Now()

			elected, err := exec.LeaderAttemptElect(ctx, &riverdriver.LeaderElectParams{
				LeaderID: clientID,
				Now:      &now,
				Schema:   "",
				TTL:      leaderTTL,
			})
			require.NoError(t, err)
			require.True(t, elected) // won election

			leader, err := exec.LeaderGetElectedLeader(ctx, &riverdriver.LeaderGetElectedLeaderParams{
				Schema: "",
			})
			require.NoError(t, err)
			require.WithinDuration(t, now, leader.ElectedAt, time.Microsecond)
			require.WithinDuration(t, now.Add(leaderTTL), leader.ExpiresAt, time.Microsecond)
		})

		t.Run("CannotElectTwiceInARow", func(t *testing.T) {
			t.Parallel()

			exec, _ := setup(ctx, t)

			leader := testfactory.Leader(ctx, t, exec, &testfactory.LeaderOpts{
				LeaderID: ptrutil.Ptr(clientID),
				Schema:   "",
			})

			elected, err := exec.LeaderAttemptElect(ctx, &riverdriver.LeaderElectParams{
				LeaderID: "different-client-id",
				Schema:   "",
				TTL:      leaderTTL,
			})
			require.NoError(t, err)
			require.False(t, elected) // lost election

			// The time should not have changed because we specified that we were not
			// already elected, and the elect query is a no-op if there's already a
			// updatedLeader:
			updatedLeader, err := exec.LeaderGetElectedLeader(ctx, &riverdriver.LeaderGetElectedLeaderParams{
				Schema: "",
			})
			require.NoError(t, err)
			require.Equal(t, leader.ExpiresAt, updatedLeader.ExpiresAt)
		})
	})

	t.Run("LeaderAttemptReelect", func(t *testing.T) {
		t.Parallel()

		t.Run("ElectsLeader", func(t *testing.T) {
			t.Parallel()

			exec, _ := setup(ctx, t)

			now := time.Now()

			elected, err := exec.LeaderAttemptReelect(ctx, &riverdriver.LeaderElectParams{
				LeaderID: clientID,
				Now:      &now,
				Schema:   "",
				TTL:      leaderTTL,
			})
			require.NoError(t, err)
			require.True(t, elected) // won election

			leader, err := exec.LeaderGetElectedLeader(ctx, &riverdriver.LeaderGetElectedLeaderParams{
				Schema: "",
			})
			require.NoError(t, err)
			require.WithinDuration(t, now, leader.ElectedAt, time.Microsecond)
			require.WithinDuration(t, now.Add(leaderTTL), leader.ExpiresAt, time.Microsecond)
		})

		t.Run("ReelectsSameLeader", func(t *testing.T) {
			t.Parallel()

			exec, _ := setup(ctx, t)

			leader := testfactory.Leader(ctx, t, exec, &testfactory.LeaderOpts{
				LeaderID: ptrutil.Ptr(clientID),
				Schema:   "",
			})

			// Re-elect the same leader. Use a larger TTL to see if time is updated,
			// because we are in a test transaction and the time is frozen at the start of
			// the transaction.
			elected, err := exec.LeaderAttemptReelect(ctx, &riverdriver.LeaderElectParams{
				LeaderID: clientID,
				Schema:   "",
				TTL:      30 * time.Second,
			})
			require.NoError(t, err)
			require.True(t, elected) // won re-election

			// expires_at should be incremented because this is the same leader that won
			// previously and we specified that we're already elected:
			updatedLeader, err := exec.LeaderGetElectedLeader(ctx, &riverdriver.LeaderGetElectedLeaderParams{
				Schema: "",
			})
			require.NoError(t, err)
			require.Greater(t, updatedLeader.ExpiresAt, leader.ExpiresAt)
		})
	})

	t.Run("LeaderDeleteExpired", func(t *testing.T) {
		t.Parallel()

		t.Run("DeletesExpired", func(t *testing.T) {
			t.Parallel()

			exec, _ := setup(ctx, t)

			now := time.Now().UTC()

			{
				numDeleted, err := exec.LeaderDeleteExpired(ctx, &riverdriver.LeaderDeleteExpiredParams{
					Schema: "",
				})
				require.NoError(t, err)
				require.Zero(t, numDeleted)
			}

			_ = testfactory.Leader(ctx, t, exec, &testfactory.LeaderOpts{
				ElectedAt: ptrutil.Ptr(now.Add(-2 * time.Hour)),
				ExpiresAt: ptrutil.Ptr(now.Add(-1 * time.Hour)),
				LeaderID:  ptrutil.Ptr(clientID),
				Schema:    "",
			})

			{
				numDeleted, err := exec.LeaderDeleteExpired(ctx, &riverdriver.LeaderDeleteExpiredParams{
					Schema: "",
				})
				require.NoError(t, err)
				require.Equal(t, 1, numDeleted)
			}
		})

		t.Run("WithInjectedNow", func(t *testing.T) {
			t.Parallel()

			exec, _ := setup(ctx, t)

			now := time.Now().UTC()

			// Elected in the future.
			_ = testfactory.Leader(ctx, t, exec, &testfactory.LeaderOpts{
				ElectedAt: ptrutil.Ptr(now.Add(1 * time.Hour)),
				ExpiresAt: ptrutil.Ptr(now.Add(2 * time.Hour)),
				LeaderID:  ptrutil.Ptr(clientID),
				Schema:    "",
			})

			numDeleted, err := exec.LeaderDeleteExpired(ctx, &riverdriver.LeaderDeleteExpiredParams{
				Now:    ptrutil.Ptr(now.Add(2*time.Hour + 1*time.Second)),
				Schema: "",
			})
			require.NoError(t, err)
			require.Equal(t, 1, numDeleted)
		})
	})

	t.Run("LeaderInsert", func(t *testing.T) {
		t.Parallel()

		exec, _ := setup(ctx, t)

		now := time.Now()

		leader, err := exec.LeaderInsert(ctx, &riverdriver.LeaderInsertParams{
			LeaderID: clientID,
			Now:      &now,
			Schema:   "",
			TTL:      leaderTTL,
		})
		require.NoError(t, err)
		require.WithinDuration(t, now, leader.ElectedAt, time.Microsecond)
		require.WithinDuration(t, now.Add(leaderTTL), leader.ExpiresAt, time.Microsecond)
		require.Equal(t, clientID, leader.LeaderID)
	})

	t.Run("LeaderGetElectedLeader", func(t *testing.T) {
		t.Parallel()

		exec, _ := setup(ctx, t)

		now := time.Now()

		_ = testfactory.Leader(ctx, t, exec, &testfactory.LeaderOpts{
			LeaderID: ptrutil.Ptr(clientID),
			Now:      &now,
			Schema:   "",
		})

		leader, err := exec.LeaderGetElectedLeader(ctx, &riverdriver.LeaderGetElectedLeaderParams{
			Schema: "",
		})
		require.NoError(t, err)
		require.WithinDuration(t, now, leader.ElectedAt, time.Microsecond)
		require.WithinDuration(t, now.Add(leaderTTL), leader.ExpiresAt, time.Microsecond)
		require.Equal(t, clientID, leader.LeaderID)
	})

	t.Run("LeaderResign", func(t *testing.T) {
		t.Parallel()

		t.Run("Success", func(t *testing.T) {
			t.Parallel()

			exec, _ := setup(ctx, t)

			{
				resigned, err := exec.LeaderResign(ctx, &riverdriver.LeaderResignParams{
					LeaderID:        clientID,
					LeadershipTopic: string(notifier.NotificationTopicLeadership),
					Schema:          "",
				})
				require.NoError(t, err)
				require.False(t, resigned)
			}

			_ = testfactory.Leader(ctx, t, exec, &testfactory.LeaderOpts{
				LeaderID: ptrutil.Ptr(clientID),
				Schema:   "",
			})

			{
				resigned, err := exec.LeaderResign(ctx, &riverdriver.LeaderResignParams{
					LeaderID:        clientID,
					LeadershipTopic: string(notifier.NotificationTopicLeadership),
					Schema:          "",
				})
				require.NoError(t, err)
				require.True(t, resigned)
			}
		})

		t.Run("DoesNotResignWithoutLeadership", func(t *testing.T) {
			t.Parallel()

			exec, _ := setup(ctx, t)

			_ = testfactory.Leader(ctx, t, exec, &testfactory.LeaderOpts{
				LeaderID: ptrutil.Ptr("other-client-id"),
				Schema:   "",
			})

			resigned, err := exec.LeaderResign(ctx, &riverdriver.LeaderResignParams{
				LeaderID:        clientID,
				LeadershipTopic: string(notifier.NotificationTopicLeadership),
				Schema:          "",
			})
			require.NoError(t, err)
			require.False(t, resigned)
		})
	})

	// Truncates the migration table so we only have to work with test
	// migration data.
	truncateMigrations := func(ctx context.Context, t *testing.T, exec riverdriver.Executor) {
		t.Helper()

		_, err := exec.Exec(ctx, "TRUNCATE TABLE river_migration")
		require.NoError(t, err)
	}

	t.Run("MigrationDeleteAssumingMainMany", func(t *testing.T) {
		t.Parallel()

		exec, _ := setup(ctx, t)

		truncateMigrations(ctx, t, exec)

		migration1 := testfactory.Migration(ctx, t, exec, &testfactory.MigrationOpts{})
		migration2 := testfactory.Migration(ctx, t, exec, &testfactory.MigrationOpts{})

		// This query is designed to work before the `line` column was added to
		// the `river_migration` table. These tests will be operating on a fully
		// migrated database, so drop the column in this transaction to make
		// sure we are really checking that this operation works as expected.
		_, err := exec.Exec(ctx, "ALTER TABLE river_migration DROP COLUMN line")
		require.NoError(t, err)

		migrations, err := exec.MigrationDeleteAssumingMainMany(ctx, &riverdriver.MigrationDeleteAssumingMainManyParams{
			Schema: "",
			Versions: []int{
				migration1.Version,
				migration2.Version,
			},
		})
		require.NoError(t, err)
		require.Len(t, migrations, 2)
		slices.SortFunc(migrations, func(a, b *riverdriver.Migration) int { return a.Version - b.Version })
		require.Equal(t, riverdriver.MigrationLineMain, migrations[0].Line)
		require.Equal(t, migration1.Version, migrations[0].Version)
		require.Equal(t, riverdriver.MigrationLineMain, migrations[1].Line)
		require.Equal(t, migration2.Version, migrations[1].Version)
	})

	t.Run("MigrationDeleteByLineAndVersionMany", func(t *testing.T) {
		t.Parallel()

		exec, _ := setup(ctx, t)

		truncateMigrations(ctx, t, exec)

		// not touched
		_ = testfactory.Migration(ctx, t, exec, &testfactory.MigrationOpts{})

		migration1 := testfactory.Migration(ctx, t, exec, &testfactory.MigrationOpts{Line: ptrutil.Ptr("alternate")})
		migration2 := testfactory.Migration(ctx, t, exec, &testfactory.MigrationOpts{Line: ptrutil.Ptr("alternate")})

		migrations, err := exec.MigrationDeleteByLineAndVersionMany(ctx, &riverdriver.MigrationDeleteByLineAndVersionManyParams{
			Line:   "alternate",
			Schema: "",
			Versions: []int{
				migration1.Version,
				migration2.Version,
			},
		})
		require.NoError(t, err)
		require.Len(t, migrations, 2)
		slices.SortFunc(migrations, func(a, b *riverdriver.Migration) int { return a.Version - b.Version })
		require.Equal(t, "alternate", migrations[0].Line)
		require.Equal(t, migration1.Version, migrations[0].Version)
		require.Equal(t, "alternate", migrations[1].Line)
		require.Equal(t, migration2.Version, migrations[1].Version)
	})

	t.Run("MigrationGetAllAssumingMain", func(t *testing.T) {
		t.Parallel()

		exec, _ := setup(ctx, t)

		truncateMigrations(ctx, t, exec)

		migration1 := testfactory.Migration(ctx, t, exec, &testfactory.MigrationOpts{})
		migration2 := testfactory.Migration(ctx, t, exec, &testfactory.MigrationOpts{})

		// This query is designed to work before the `line` column was added to
		// the `river_migration` table. These tests will be operating on a fully
		// migrated database, so drop the column in this transaction to make
		// sure we are really checking that this operation works as expected.
		_, err := exec.Exec(ctx, "ALTER TABLE river_migration DROP COLUMN line")
		require.NoError(t, err)

		migrations, err := exec.MigrationGetAllAssumingMain(ctx, &riverdriver.MigrationGetAllAssumingMainParams{
			Schema: "",
		})
		require.NoError(t, err)
		require.Len(t, migrations, 2)
		require.Equal(t, migration1.Version, migrations[0].Version)
		require.Equal(t, migration2.Version, migrations[1].Version)

		// Check the full properties of one of the migrations.
		migration1Fetched := migrations[0]
		requireEqualTime(t, migration1.CreatedAt, migration1Fetched.CreatedAt)
		require.Equal(t, riverdriver.MigrationLineMain, migration1Fetched.Line)
		require.Equal(t, migration1.Version, migration1Fetched.Version)
	})

	t.Run("MigrationGetByLine", func(t *testing.T) {
		t.Parallel()

		exec, _ := setup(ctx, t)

		truncateMigrations(ctx, t, exec)

		// not returned
		_ = testfactory.Migration(ctx, t, exec, &testfactory.MigrationOpts{})

		migration1 := testfactory.Migration(ctx, t, exec, &testfactory.MigrationOpts{Line: ptrutil.Ptr("alternate")})
		migration2 := testfactory.Migration(ctx, t, exec, &testfactory.MigrationOpts{Line: ptrutil.Ptr("alternate")})

		migrations, err := exec.MigrationGetByLine(ctx, &riverdriver.MigrationGetByLineParams{
			Line:   "alternate",
			Schema: "",
		})
		require.NoError(t, err)
		require.Len(t, migrations, 2)
		require.Equal(t, migration1.Version, migrations[0].Version)
		require.Equal(t, migration2.Version, migrations[1].Version)

		// Check the full properties of one of the migrations.
		migration1Fetched := migrations[0]
		requireEqualTime(t, migration1.CreatedAt, migration1Fetched.CreatedAt)
		require.Equal(t, "alternate", migration1Fetched.Line)
		require.Equal(t, migration1.Version, migration1Fetched.Version)
	})

	t.Run("MigrationInsertMany", func(t *testing.T) {
		t.Parallel()

		exec, _ := setup(ctx, t)

		truncateMigrations(ctx, t, exec)

		migrations, err := exec.MigrationInsertMany(ctx, &riverdriver.MigrationInsertManyParams{
			Line:     "alternate",
			Versions: []int{1, 2},
		})
		require.NoError(t, err)
		require.Len(t, migrations, 2)
		require.Equal(t, "alternate", migrations[0].Line)
		require.Equal(t, 1, migrations[0].Version)
		require.Equal(t, "alternate", migrations[1].Line)
		require.Equal(t, 2, migrations[1].Version)
	})

	t.Run("MigrationInsertManyAssumingMain", func(t *testing.T) {
		t.Parallel()

		exec, _ := setup(ctx, t)

		truncateMigrations(ctx, t, exec)

		// This query is designed to work before the `line` column was added to
		// the `river_migration` table. These tests will be operating on a fully
		// migrated database, so drop the column in this transaction to make
		// sure we are really checking that this operation works as expected.
		_, err := exec.Exec(ctx, "ALTER TABLE river_migration DROP COLUMN line")
		require.NoError(t, err)

		migrations, err := exec.MigrationInsertManyAssumingMain(ctx, &riverdriver.MigrationInsertManyAssumingMainParams{
			Schema:   "",
			Versions: []int{1, 2},
		})

		require.NoError(t, err)
		require.Len(t, migrations, 2)
		require.Equal(t, riverdriver.MigrationLineMain, migrations[0].Line)
		require.Equal(t, 1, migrations[0].Version)
		require.Equal(t, riverdriver.MigrationLineMain, migrations[1].Line)
		require.Equal(t, 2, migrations[1].Version)
	})

	t.Run("TableExists", func(t *testing.T) {
		t.Parallel()

		exec, _ := setup(ctx, t)

		exists, err := exec.TableExists(ctx, &riverdriver.TableExistsParams{
			Schema: "",
			Table:  "river_job",
		})
		require.NoError(t, err)
		require.True(t, exists)

		exists, err = exec.TableExists(ctx, &riverdriver.TableExistsParams{
			Schema: "",
			Table:  "does_not_exist",
		})
		require.NoError(t, err)
		require.False(t, exists)

		// Will be rolled back by the test transaction.
		_, err = exec.Exec(ctx, "CREATE SCHEMA another_schema_123")
		require.NoError(t, err)

		_, err = exec.Exec(ctx, "SET search_path = another_schema_123")
		require.NoError(t, err)

		exists, err = exec.TableExists(ctx, &riverdriver.TableExistsParams{
			Schema: "",
			Table:  "river_job",
		})
		require.NoError(t, err)
		require.False(t, exists)
	})

	t.Run("PGAdvisoryXactLock", func(t *testing.T) {
		t.Parallel()

		exec, _ := setup(ctx, t)

		// Acquire the advisory lock.
		_, err := exec.PGAdvisoryXactLock(ctx, 123456)
		require.NoError(t, err)

		// Open a new transaction and try to acquire the same lock, which should
		// block because the lock can't be acquired. Verify some amount of wait,
		// cancel the lock acquisition attempt, then verify return.
		{
			otherExec := executorWithTx(ctx, t)

			goroutineDone := make(chan struct{})

			ctx, cancel := context.WithCancel(ctx)
			t.Cleanup(cancel)

			go func() {
				defer close(goroutineDone)

				_, err := otherExec.PGAdvisoryXactLock(ctx, 123456)
				require.ErrorIs(t, err, context.Canceled)
			}()

			select {
			case <-goroutineDone:
				require.FailNow(t, "Unexpectedly acquired lock that should've held by other transaction")
			case <-time.After(50 * time.Millisecond):
			}

			cancel()

			select {
			case <-goroutineDone:
			case <-time.After(50 * time.Millisecond):
				require.FailNow(t, "Goroutine didn't finish in a timely manner")
			}
		}
	})

	t.Run("QueueCreateOrSetUpdatedAt", func(t *testing.T) {
		t.Run("InsertsANewQueueWithDefaultUpdatedAt", func(t *testing.T) {
			t.Parallel()

			exec, _ := setup(ctx, t)

			metadata := []byte(`{"foo": "bar"}`)
			now := time.Now().UTC()
			queue, err := exec.QueueCreateOrSetUpdatedAt(ctx, &riverdriver.QueueCreateOrSetUpdatedAtParams{
				Metadata: metadata,
				Name:     "new-queue",
				Now:      &now,
				Schema:   "",
			})
			require.NoError(t, err)
			require.WithinDuration(t, now, queue.CreatedAt, time.Microsecond)
			require.Equal(t, metadata, queue.Metadata)
			require.Equal(t, "new-queue", queue.Name)
			require.Nil(t, queue.PausedAt)
			require.WithinDuration(t, now, queue.UpdatedAt, time.Microsecond)
		})

		t.Run("InsertsANewQueueWithCustomPausedAt", func(t *testing.T) {
			t.Parallel()

			exec, _ := setup(ctx, t)

			now := time.Now().Add(-5 * time.Minute)
			queue, err := exec.QueueCreateOrSetUpdatedAt(ctx, &riverdriver.QueueCreateOrSetUpdatedAtParams{
				Name:     "new-queue",
				PausedAt: ptrutil.Ptr(now),
				Schema:   "",
			})
			require.NoError(t, err)
			require.Equal(t, "new-queue", queue.Name)
			require.WithinDuration(t, now, *queue.PausedAt, time.Millisecond)
		})

		t.Run("UpdatesTheUpdatedAtOfExistingQueue", func(t *testing.T) {
			t.Parallel()

			exec, _ := setup(ctx, t)

			metadata := []byte(`{"foo": "bar"}`)
			tBefore := time.Now().UTC()
			queueBefore, err := exec.QueueCreateOrSetUpdatedAt(ctx, &riverdriver.QueueCreateOrSetUpdatedAtParams{
				Metadata:  metadata,
				Name:      "updatable-queue",
				Schema:    "",
				UpdatedAt: &tBefore,
			})
			require.NoError(t, err)
			require.WithinDuration(t, tBefore, queueBefore.UpdatedAt, time.Millisecond)

			tAfter := tBefore.Add(2 * time.Second)
			queueAfter, err := exec.QueueCreateOrSetUpdatedAt(ctx, &riverdriver.QueueCreateOrSetUpdatedAtParams{
				Metadata:  []byte(`{"other": "metadata"}`),
				Name:      "updatable-queue",
				Schema:    "",
				UpdatedAt: &tAfter,
			})
			require.NoError(t, err)

			// unchanged:
			require.Equal(t, queueBefore.CreatedAt, queueAfter.CreatedAt)
			require.Equal(t, metadata, queueAfter.Metadata)
			require.Equal(t, "updatable-queue", queueAfter.Name)
			require.Nil(t, queueAfter.PausedAt)

			// Timestamp is bumped:
			require.WithinDuration(t, tAfter, queueAfter.UpdatedAt, time.Millisecond)
		})
	})

	t.Run("QueueDeleteExpired", func(t *testing.T) {
		t.Parallel()

		exec, _ := setup(ctx, t)

		now := time.Now()
		_ = testfactory.Queue(ctx, t, exec, &testfactory.QueueOpts{UpdatedAt: ptrutil.Ptr(now)})
		queue2 := testfactory.Queue(ctx, t, exec, &testfactory.QueueOpts{UpdatedAt: ptrutil.Ptr(now.Add(-25 * time.Hour))})
		queue3 := testfactory.Queue(ctx, t, exec, &testfactory.QueueOpts{UpdatedAt: ptrutil.Ptr(now.Add(-26 * time.Hour))})
		queue4 := testfactory.Queue(ctx, t, exec, &testfactory.QueueOpts{UpdatedAt: ptrutil.Ptr(now.Add(-48 * time.Hour))})
		_ = testfactory.Queue(ctx, t, exec, &testfactory.QueueOpts{UpdatedAt: ptrutil.Ptr(now.Add(-23 * time.Hour))})

		horizon := now.Add(-24 * time.Hour)
		deletedQueueNames, err := exec.QueueDeleteExpired(ctx, &riverdriver.QueueDeleteExpiredParams{Max: 2, UpdatedAtHorizon: horizon})
		require.NoError(t, err)

		// queue2 and queue3 should be deleted, with queue4 being skipped due to max of 2:
		require.Equal(t, []string{queue2.Name, queue3.Name}, deletedQueueNames)

		// Try again, make sure queue4 gets deleted this time:
		deletedQueueNames, err = exec.QueueDeleteExpired(ctx, &riverdriver.QueueDeleteExpiredParams{Max: 2, UpdatedAtHorizon: horizon})
		require.NoError(t, err)

		require.Equal(t, []string{queue4.Name}, deletedQueueNames)
	})

	t.Run("QueueGet", func(t *testing.T) {
		t.Parallel()

		exec, _ := setup(ctx, t)

		queue := testfactory.Queue(ctx, t, exec, &testfactory.QueueOpts{Metadata: []byte(`{"foo": "bar"}`)})

		queueFetched, err := exec.QueueGet(ctx, &riverdriver.QueueGetParams{
			Name:   queue.Name,
			Schema: "",
		})
		require.NoError(t, err)

		require.WithinDuration(t, queue.CreatedAt, queueFetched.CreatedAt, time.Millisecond)
		require.Equal(t, queue.Metadata, queueFetched.Metadata)
		require.Equal(t, queue.Name, queueFetched.Name)
		require.Nil(t, queueFetched.PausedAt)
		require.WithinDuration(t, queue.UpdatedAt, queueFetched.UpdatedAt, time.Millisecond)

		queueFetched, err = exec.QueueGet(ctx, &riverdriver.QueueGetParams{
			Name:   "nonexistent-queue",
			Schema: "",
		})
		require.ErrorIs(t, err, rivertype.ErrNotFound)
		require.Nil(t, queueFetched)
	})

	t.Run("QueueList", func(t *testing.T) {
		t.Parallel()

		exec, _ := setup(ctx, t)

		requireQueuesEqual := func(t *testing.T, target, actual *rivertype.Queue) {
			t.Helper()
			require.WithinDuration(t, target.CreatedAt, actual.CreatedAt, time.Millisecond)
			require.Equal(t, target.Metadata, actual.Metadata)
			require.Equal(t, target.Name, actual.Name)
			if target.PausedAt == nil {
				require.Nil(t, actual.PausedAt)
			} else {
				require.NotNil(t, actual.PausedAt)
				require.WithinDuration(t, *target.PausedAt, *actual.PausedAt, time.Millisecond)
			}
		}

		queues, err := exec.QueueList(ctx, &riverdriver.QueueListParams{
			Limit:  10,
			Schema: "",
		})
		require.NoError(t, err)
		require.Empty(t, queues)

		// Make queue1, already paused:
		queue1 := testfactory.Queue(ctx, t, exec, &testfactory.QueueOpts{Metadata: []byte(`{"foo": "bar"}`), PausedAt: ptrutil.Ptr(time.Now())})
		require.NoError(t, err)

		queue2 := testfactory.Queue(ctx, t, exec, nil)
		queue3 := testfactory.Queue(ctx, t, exec, nil)

		queues, err = exec.QueueList(ctx, &riverdriver.QueueListParams{
			Limit:  2,
			Schema: "",
		})
		require.NoError(t, err)

		require.Len(t, queues, 2)
		requireQueuesEqual(t, queue1, queues[0])
		requireQueuesEqual(t, queue2, queues[1])

		queues, err = exec.QueueList(ctx, &riverdriver.QueueListParams{
			Limit:  3,
			Schema: "",
		})
		require.NoError(t, err)

		require.Len(t, queues, 3)
		requireQueuesEqual(t, queue3, queues[2])
	})

	t.Run("QueuePause", func(t *testing.T) {
		t.Parallel()

		t.Run("ExistingPausedQueue", func(t *testing.T) {
			t.Parallel()

			exec, _ := setup(ctx, t)

			queue := testfactory.Queue(ctx, t, exec, &testfactory.QueueOpts{
				PausedAt: ptrutil.Ptr(time.Now()),
			})

			require.NoError(t, exec.QueuePause(ctx, &riverdriver.QueuePauseParams{
				Name:   queue.Name,
				Schema: "",
			}))
			queueFetched, err := exec.QueueGet(ctx, &riverdriver.QueueGetParams{
				Name:   queue.Name,
				Schema: "",
			})
			require.NoError(t, err)
			require.NotNil(t, queueFetched.PausedAt)
			requireEqualTime(t, *queue.PausedAt, *queueFetched.PausedAt) // paused_at stays unchanged
			requireEqualTime(t, queue.UpdatedAt, queueFetched.UpdatedAt) // updated_at stays unchanged
		})

		t.Run("ExistingUnpausedQueue", func(t *testing.T) {
			t.Parallel()

			exec, _ := setup(ctx, t)

			queue := testfactory.Queue(ctx, t, exec, nil)
			require.Nil(t, queue.PausedAt)

			require.NoError(t, exec.QueuePause(ctx, &riverdriver.QueuePauseParams{
				Name:   queue.Name,
				Schema: "",
			}))

			queueFetched, err := exec.QueueGet(ctx, &riverdriver.QueueGetParams{
				Name:   queue.Name,
				Schema: "",
			})
			require.NoError(t, err)
			require.NotNil(t, queueFetched.PausedAt)
			require.WithinDuration(t, time.Now(), *(queueFetched.PausedAt), 500*time.Millisecond)
		})

		t.Run("NonExistentQueue", func(t *testing.T) {
			t.Parallel()

			exec, _ := setup(ctx, t)

			err := exec.QueuePause(ctx, &riverdriver.QueuePauseParams{
				Name:   "queue1",
				Schema: "",
			})
			require.ErrorIs(t, err, rivertype.ErrNotFound)
		})

		t.Run("AllQueuesExistingQueues", func(t *testing.T) {
			t.Parallel()

			exec, _ := setup(ctx, t)

			queue1 := testfactory.Queue(ctx, t, exec, nil)
			require.Nil(t, queue1.PausedAt)
			queue2 := testfactory.Queue(ctx, t, exec, nil)
			require.Nil(t, queue2.PausedAt)

			require.NoError(t, exec.QueuePause(ctx, &riverdriver.QueuePauseParams{
				Name:   rivercommon.AllQueuesString,
				Schema: "",
			}))

			now := time.Now()

			queue1Fetched, err := exec.QueueGet(ctx, &riverdriver.QueueGetParams{
				Name:   queue1.Name,
				Schema: "",
			})
			require.NoError(t, err)
			require.NotNil(t, queue1Fetched.PausedAt)
			require.WithinDuration(t, now, *(queue1Fetched.PausedAt), 500*time.Millisecond)

			queue2Fetched, err := exec.QueueGet(ctx, &riverdriver.QueueGetParams{
				Name:   queue2.Name,
				Schema: "",
			})
			require.NoError(t, err)
			require.NotNil(t, queue2Fetched.PausedAt)
			require.WithinDuration(t, now, *(queue2Fetched.PausedAt), 500*time.Millisecond)
		})

		t.Run("AllQueuesNoQueues", func(t *testing.T) {
			t.Parallel()

			exec, _ := setup(ctx, t)

			require.NoError(t, exec.QueuePause(ctx, &riverdriver.QueuePauseParams{
				Name:   rivercommon.AllQueuesString,
				Schema: "",
			}))
		})
	})

	t.Run("QueueResume", func(t *testing.T) {
		t.Parallel()

		t.Run("ExistingPausedQueue", func(t *testing.T) {
			t.Parallel()

			exec, _ := setup(ctx, t)

			queue := testfactory.Queue(ctx, t, exec, &testfactory.QueueOpts{
				PausedAt: ptrutil.Ptr(time.Now()),
			})

			require.NoError(t, exec.QueueResume(ctx, &riverdriver.QueueResumeParams{
				Name:   queue.Name,
				Schema: "",
			}))

			queueFetched, err := exec.QueueGet(ctx, &riverdriver.QueueGetParams{
				Name:   queue.Name,
				Schema: "",
			})
			require.NoError(t, err)
			require.Nil(t, queueFetched.PausedAt)
		})

		t.Run("ExistingUnpausedQueue", func(t *testing.T) {
			t.Parallel()

			exec, _ := setup(ctx, t)

			queue := testfactory.Queue(ctx, t, exec, nil)

			require.NoError(t, exec.QueueResume(ctx, &riverdriver.QueueResumeParams{
				Name:   queue.Name,
				Schema: "",
			}))

			queueFetched, err := exec.QueueGet(ctx, &riverdriver.QueueGetParams{
				Name:   queue.Name,
				Schema: "",
			})
			require.NoError(t, err)
			require.Nil(t, queueFetched.PausedAt)
			requireEqualTime(t, queue.UpdatedAt, queueFetched.UpdatedAt) // updated_at stays unchanged
		})

		t.Run("NonExistentQueue", func(t *testing.T) {
			t.Parallel()

			exec, _ := setup(ctx, t)

			err := exec.QueueResume(ctx, &riverdriver.QueueResumeParams{
				Name:   "queue1",
				Schema: "",
			})
			require.ErrorIs(t, err, rivertype.ErrNotFound)
		})

		t.Run("AllQueuesExistingQueues", func(t *testing.T) {
			t.Parallel()

			exec, _ := setup(ctx, t)

			queue1 := testfactory.Queue(ctx, t, exec, nil)
			require.Nil(t, queue1.PausedAt)
			queue2 := testfactory.Queue(ctx, t, exec, nil)
			require.Nil(t, queue2.PausedAt)

			require.NoError(t, exec.QueuePause(ctx, &riverdriver.QueuePauseParams{
				Name:   rivercommon.AllQueuesString,
				Schema: "",
			}))
			require.NoError(t, exec.QueueResume(ctx, &riverdriver.QueueResumeParams{
				Name:   rivercommon.AllQueuesString,
				Schema: "",
			}))

			queue1Fetched, err := exec.QueueGet(ctx, &riverdriver.QueueGetParams{
				Name:   queue1.Name,
				Schema: "",
			})
			require.NoError(t, err)
			require.Nil(t, queue1Fetched.PausedAt)

			queue2Fetched, err := exec.QueueGet(ctx, &riverdriver.QueueGetParams{
				Name:   queue2.Name,
				Schema: "",
			})
			require.NoError(t, err)
			require.Nil(t, queue2Fetched.PausedAt)
		})

		t.Run("AllQueuesNoQueues", func(t *testing.T) {
			t.Parallel()

			exec, _ := setup(ctx, t)

			require.NoError(t, exec.QueueResume(ctx, &riverdriver.QueueResumeParams{
				Name:   rivercommon.AllQueuesString,
				Schema: "",
			}))
		})
	})

	t.Run("QueueUpdate", func(t *testing.T) {
		t.Parallel()

		t.Run("UpdatesFieldsIfDoUpdateIsTrue", func(t *testing.T) {
			t.Parallel()

			exec, _ := setup(ctx, t)

			queue := testfactory.Queue(ctx, t, exec, &testfactory.QueueOpts{Metadata: []byte(`{"foo": "bar"}`)})

			updatedQueue, err := exec.QueueUpdate(ctx, &riverdriver.QueueUpdateParams{
				Metadata:         []byte(`{"baz": "qux"}`),
				MetadataDoUpdate: true,
				Name:             queue.Name,
			})
			require.NoError(t, err)
			require.JSONEq(t, `{"baz": "qux"}`, string(updatedQueue.Metadata))
		})

		t.Run("DoesNotUpdateFieldsIfDoUpdateIsFalse", func(t *testing.T) {
			t.Parallel()

			exec, _ := setup(ctx, t)

			queue := testfactory.Queue(ctx, t, exec, &testfactory.QueueOpts{Metadata: []byte(`{"foo": "bar"}`)})

			updatedQueue, err := exec.QueueUpdate(ctx, &riverdriver.QueueUpdateParams{
				Metadata:         []byte(`{"baz": "qux"}`),
				MetadataDoUpdate: false,
				Name:             queue.Name,
			})
			require.NoError(t, err)
			require.JSONEq(t, `{"foo": "bar"}`, string(updatedQueue.Metadata))
		})
	})

	t.Run("QueryRow", func(t *testing.T) {
		t.Parallel()

		exec, _ := setup(ctx, t)

		var (
			field1   int
			field2   int
			field3   int
			fieldFoo string
		)

		err := exec.QueryRow(ctx, "SELECT 1, 2, 3, 'foo'").Scan(&field1, &field2, &field3, &fieldFoo)
		require.NoError(t, err)

		require.Equal(t, 1, field1)
		require.Equal(t, 2, field2)
		require.Equal(t, 3, field3)
		require.Equal(t, "foo", fieldFoo)
	})

	t.Run("SchemaGetExpired", func(t *testing.T) {
		t.Parallel()

		t.Run("FiltersSchemasNotMatchingPrefix", func(t *testing.T) {
			t.Parallel()

			exec, _ := setup(ctx, t)

			schemas, err := exec.SchemaGetExpired(ctx, &riverdriver.SchemaGetExpiredParams{
				BeforeName: "zzzzzzzzzzzzzzzzzz",
				Prefix:     "this_prefix_will_not_exist_",
			})
			require.NoError(t, err)
			require.Empty(t, schemas)
		})

		t.Run("ListsSchemasBelowMarker", func(t *testing.T) {
			t.Parallel()

			exec, _ := setup(ctx, t)

			schemas, err := exec.SchemaGetExpired(ctx, &riverdriver.SchemaGetExpiredParams{
				BeforeName: "pg_toast",
				Prefix:     "pg_%",
			})
			require.NoError(t, err)
			require.Equal(t, []string{"pg_catalog"}, schemas)
		})
	})
}

type testListenerBundle[TTx any] struct {
	driver riverdriver.Driver[TTx]
	exec   riverdriver.Executor
}

func setupListener[TTx any](ctx context.Context, t *testing.T, driverWithPool func(ctx context.Context, t *testing.T) (riverdriver.Driver[TTx], string)) (riverdriver.Listener, *testListenerBundle[TTx]) {
	t.Helper()

	var (
		driver, schema = driverWithPool(ctx, t)
		listener       = driver.GetListener(&riverdriver.GetListenenerParams{Schema: schema})
	)

	return listener, &testListenerBundle[TTx]{
		driver: driver,
		exec:   driver.GetExecutor(),
	}
}

func exerciseListener[TTx any](ctx context.Context, t *testing.T, driverWithPool func(ctx context.Context, t *testing.T) (riverdriver.Driver[TTx], string)) {
	t.Helper()

	connectListener := func(ctx context.Context, t *testing.T, listener riverdriver.Listener) {
		t.Helper()

		require.NoError(t, listener.Connect(ctx))
		t.Cleanup(func() { require.NoError(t, listener.Close(ctx)) })
	}

	requireNoNotification := func(ctx context.Context, t *testing.T, listener riverdriver.Listener) {
		t.Helper()

		// Ugh, this is a little sketchy, but hard to test in another way.
		ctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
		defer cancel()

		notification, err := listener.WaitForNotification(ctx)
		require.ErrorIs(t, err, context.DeadlineExceeded, "Expected no notification, but got: %+v", notification)
	}

	waitForNotification := func(ctx context.Context, t *testing.T, listener riverdriver.Listener) *riverdriver.Notification {
		t.Helper()

		ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()

		notification, err := listener.WaitForNotification(ctx)
		require.NoError(t, err)

		return notification
	}

	t.Run("Close_NoOpIfNotConnected", func(t *testing.T) {
		t.Parallel()

		listener, _ := setupListener(ctx, t, driverWithPool)
		require.NoError(t, listener.Close(ctx))
	})

	t.Run("RoundTrip", func(t *testing.T) {
		t.Parallel()

		listener, bundle := setupListener(ctx, t, driverWithPool)

		connectListener(ctx, t, listener)

		require.NoError(t, listener.Listen(ctx, "topic1"))
		require.NoError(t, listener.Listen(ctx, "topic2"))

		require.NoError(t, listener.Ping(ctx)) // still alive

		{
			require.NoError(t, bundle.exec.NotifyMany(ctx, &riverdriver.NotifyManyParams{Topic: "topic1", Payload: []string{"payload1_1"}, Schema: listener.Schema()}))
			require.NoError(t, bundle.exec.NotifyMany(ctx, &riverdriver.NotifyManyParams{Topic: "topic2", Payload: []string{"payload2_1"}, Schema: listener.Schema()}))

			notification := waitForNotification(ctx, t, listener)
			require.Equal(t, &riverdriver.Notification{Topic: "topic1", Payload: "payload1_1"}, notification)
			notification = waitForNotification(ctx, t, listener)
			require.Equal(t, &riverdriver.Notification{Topic: "topic2", Payload: "payload2_1"}, notification)
		}

		require.NoError(t, listener.Unlisten(ctx, "topic2"))

		{
			require.NoError(t, bundle.exec.NotifyMany(ctx, &riverdriver.NotifyManyParams{Topic: "topic1", Payload: []string{"payload1_2"}, Schema: listener.Schema()}))
			require.NoError(t, bundle.exec.NotifyMany(ctx, &riverdriver.NotifyManyParams{Topic: "topic2", Payload: []string{"payload2_2"}, Schema: listener.Schema()}))

			notification := waitForNotification(ctx, t, listener)
			require.Equal(t, &riverdriver.Notification{Topic: "topic1", Payload: "payload1_2"}, notification)

			requireNoNotification(ctx, t, listener)
		}

		require.NoError(t, listener.Unlisten(ctx, "topic1"))

		require.NoError(t, listener.Close(ctx))
	})

	t.Run("SchemaFromParameter", func(t *testing.T) {
		t.Parallel()

		var (
			driver, _ = driverWithPool(ctx, t)
			listener  = driver.GetListener(&riverdriver.GetListenenerParams{Schema: "my_custom_schema"})
		)

		require.Equal(t, "my_custom_schema", listener.Schema())
	})

	t.Run("SchemaFromSearchPath", func(t *testing.T) {
		t.Parallel()

		var (
			driver, _ = driverWithPool(ctx, t)
			listener  = driver.GetListener(&riverdriver.GetListenenerParams{Schema: ""})
		)

		listener.SetAfterConnectExec("SET search_path TO 'public'")

		connectListener(ctx, t, listener)
		require.Equal(t, "public", listener.Schema())
	})

	t.Run("EmptySchemaFromSearchPath", func(t *testing.T) {
		t.Parallel()

		var (
			driver, _ = driverWithPool(ctx, t)
			listener  = driver.GetListener(&riverdriver.GetListenenerParams{Schema: ""})
		)

		connectListener(ctx, t, listener)
		require.Empty(t, listener.Schema())
	})

	t.Run("TransactionGated", func(t *testing.T) {
		t.Parallel()

		listener, bundle := setupListener(ctx, t, driverWithPool)

		connectListener(ctx, t, listener)

		require.NoError(t, listener.Listen(ctx, "topic1"))

		tx, err := bundle.exec.Begin(ctx)
		require.NoError(t, err)

		require.NoError(t, tx.NotifyMany(ctx, &riverdriver.NotifyManyParams{Topic: "topic1", Payload: []string{"payload1"}, Schema: listener.Schema()}))

		// No notification because the transaction hasn't committed yet.
		requireNoNotification(ctx, t, listener)

		require.NoError(t, tx.Commit(ctx))

		// Notification received now that transaction has committed.
		notification := waitForNotification(ctx, t, listener)
		require.Equal(t, &riverdriver.Notification{Topic: "topic1", Payload: "payload1"}, notification)
	})

	t.Run("MultipleReuse", func(t *testing.T) {
		t.Parallel()

		listener, _ := setupListener(ctx, t, driverWithPool)

		connectListener(ctx, t, listener)

		require.NoError(t, listener.Listen(ctx, "topic1"))
		require.NoError(t, listener.Unlisten(ctx, "topic1"))

		require.NoError(t, listener.Close(ctx))
		require.NoError(t, listener.Connect(ctx))

		require.NoError(t, listener.Listen(ctx, "topic1"))
		require.NoError(t, listener.Unlisten(ctx, "topic1"))
	})
}

// requireEqualTime compares to timestamps down the microsecond only. This is
// appropriate for comparing times that might've roundtripped from Postgres,
// which only stores to microsecond precision.
func requireEqualTime(t *testing.T, expected, actual time.Time) {
	t.Helper()

	// Leaving off the nanosecond portion has the effect of truncating it rather
	// than rounding to the nearest microsecond, which functionally matches
	// pgx's behavior while persisting.
	const rfc3339Micro = "2006-01-02T15:04:05.999999Z07:00"

	require.Equal(t,
		expected.Format(rfc3339Micro),
		actual.Format(rfc3339Micro),
	)
}

func requireMissingRelation(t *testing.T, err error, missingRelation string) {
	t.Helper()

	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr)
	require.Equal(t, pgerrcode.UndefinedTable, pgErr.Code)
	require.Equal(t, fmt.Sprintf(`relation "%s" does not exist`, missingRelation), pgErr.Message)
}
