/*
Copyright 2023 The Vitess Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package cli

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math"
	"math/big"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"vitess.io/vitess/go/acl"
	"vitess.io/vitess/go/cmd"
	"vitess.io/vitess/go/exit"
	"vitess.io/vitess/go/mysql/collations"
	"vitess.io/vitess/go/mysql/replication"
	"vitess.io/vitess/go/stats"
	"vitess.io/vitess/go/vt/dbconfigs"
	"vitess.io/vitess/go/vt/log"
	"vitess.io/vitess/go/vt/logutil"
	"vitess.io/vitess/go/vt/mysqlctl"
	"vitess.io/vitess/go/vt/mysqlctl/backupstats"
	"vitess.io/vitess/go/vt/mysqlctl/backupstorage"
	topodatapb "vitess.io/vitess/go/vt/proto/topodata"
	"vitess.io/vitess/go/vt/servenv"
	"vitess.io/vitess/go/vt/topo"
	"vitess.io/vitess/go/vt/topo/topoproto"
	"vitess.io/vitess/go/vt/utils"
	"vitess.io/vitess/go/vt/vterrors"
	_ "vitess.io/vitess/go/vt/vttablet/grpctmclient"
	"vitess.io/vitess/go/vt/vttablet/tmclient"
)

const (
	// operationTimeout is the timeout for individual operations like fetching
	// the primary position. This does not impose an overall timeout on
	// long-running processes like taking the backup. It only applies to
	// steps along the way that should complete quickly. This ensures we don't
	// place a hard cap on the overall time for a backup, while also not waiting
	// forever for things that should be quick.
	operationTimeout = 1 * time.Minute

	phaseNameCatchupReplication          = "CatchupReplication"
	phaseNameInitialBackup               = "InitialBackup"
	phaseNameRestoreLastBackup           = "RestoreLastBackup"
	phaseNameTakeNewBackup               = "TakeNewBackup"
	phaseStatusCatchupReplicationStalled = "Stalled"
	phaseStatusCatchupReplicationStopped = "Stopped"

	timeoutWaitingForReplicationStatus = 60 * time.Second
)

var (
	minBackupInterval   time.Duration
	minRetentionTime    time.Duration
	minRetentionCount   = 1
	initialBackup       bool
	allowFirstBackup    bool
	restartBeforeBackup bool
	upgradeSafe         bool

	// vttablet-like flags
	initDbNameOverride string
	initKeyspace       string
	initShard          string
	concurrency        = 4
	incrementalFromPos string

	// mysqlctld-like flags
	mysqlPort            = 3306
	mysqlSocket          string
	mysqlTimeout         = 5 * time.Minute
	mysqlShutdownTimeout = mysqlctl.DefaultShutdownTimeout
	initDBSQLFile        string
	detachedMode         bool
	keepAliveTimeout     time.Duration
	disableRedoLog       bool

	collationEnv *collations.Environment

	// Deprecated, use "Phase" instead.
	deprecatedDurationByPhase = stats.NewGaugesWithSingleLabel(
		"DurationByPhaseSeconds",
		"[DEPRECATED] How long it took vtbackup to perform each phase (in seconds).",
		"phase",
	)

	// This gauge is updated 3*N times during the course of a vtbackup run,
	// where N is the number of different phases vtbackup transitions through.
	// Once to initialize to 0, another time to set the phase to active (1),
	// and another to deactivate the phase (back to 0).
	//
	// At most a single phase is active at a given time.
	//
	// The sync gauge immediately reports changes to push-backed backends.
	// The benefit of the sync gauge is that it makes verifying stats in
	// integration tests a lot more tractable.
	phase = stats.NewSyncGaugesWithSingleLabel(
		"Phase",
		"Active phase.",
		"phase",
	)
	phaseNames = []string{
		phaseNameCatchupReplication,
		phaseNameInitialBackup,
		phaseNameRestoreLastBackup,
		phaseNameTakeNewBackup,
	}
	phaseStatus = stats.NewGaugesWithMultiLabels(
		"PhaseStatus",
		"Internal state of vtbackup phase.",
		[]string{"phase", "status"},
	)
	phaseStatuses = map[string][]string{
		phaseNameCatchupReplication: {
			phaseStatusCatchupReplicationStalled,
			phaseStatusCatchupReplicationStopped,
		},
	}

	Main = &cobra.Command{
		Use:   "vtbackup",
		Short: "vtbackup is a batch command to perform a single pass of backup maintenance for a shard.",
		Long: `vtbackup is a batch command to perform a single pass of backup maintenance for a shard.

When run periodically for each shard, vtbackup can ensure these configurable policies:
 * There is always a recent backup for the shard.

 * Old backups for the shard are removed.

Whatever system launches vtbackup is responsible for the following:
 - Running vtbackup with similar flags that would be used for a vttablet and 
   mysqlctld in the target shard to be backed up.

 - Provisioning as much disk space for vtbackup as would be given to vttablet.
   The data directory MUST be empty at startup. Do NOT reuse a persistent disk.

 - Running vtbackup periodically for each shard, for each backup storage location.

 - Ensuring that at most one instance runs at a time for a given pair of shard
   and backup storage location.

 - Retrying vtbackup if it fails.

 - Alerting human operators if the failure is persistent.

The process vtbackup follows to take a new backup has the following steps:
 1. Restore from the most recent backup.
 2. Start a mysqld instance (but no vttablet) from the restored data.
 3. Instruct mysqld to connect to the current shard primary and replicate any
    transactions that are new since the last backup.
 4. Ask the primary for its current replication position and set that as the goal
    for catching up on replication before taking the backup, so the goalposts
    don't move.
 5. Wait until replication is caught up to the goal position or beyond.
 6. Stop mysqld and take a new backup.

Aside from additional replication load while vtbackup's mysqld catches up on
new transactions, the shard should be otherwise unaffected. Existing tablets
will continue to serve, and no new tablets will appear in topology, meaning no
query traffic will ever be routed to vtbackup's mysqld. This silent operation
mode helps make backups minimally disruptive to serving capacity and orthogonal
to the handling of the query path.

The command-line parameters to vtbackup specify a policy for when a new backup
is needed, and when old backups should be removed. If the existing backups
already satisfy the policy, then vtbackup will do nothing and return success
immediately.`,
		Version: servenv.AppVersion.String(),
		Args:    cobra.NoArgs,
		PreRunE: servenv.CobraPreRunE,
		RunE:    run,
	}
)

func init() {
	servenv.RegisterDefaultFlags()
	dbconfigs.RegisterFlags(dbconfigs.All...)
	mysqlctl.RegisterFlags()

	servenv.MoveFlagsToCobraCommand(Main)

	Main.Flags().DurationVar(&minBackupInterval, "min_backup_interval", minBackupInterval, "Only take a new backup if it's been at least this long since the most recent backup.")
	Main.Flags().DurationVar(&minRetentionTime, "min_retention_time", minRetentionTime, "Keep each old backup for at least this long before removing it. Set to 0 to disable pruning of old backups.")
	Main.Flags().IntVar(&minRetentionCount, "min_retention_count", minRetentionCount, "Always keep at least this many of the most recent backups in this backup storage location, even if some are older than the min_retention_time. This must be at least 1 since a backup must always exist to allow new backups to be made")
	Main.Flags().BoolVar(&initialBackup, "initial_backup", initialBackup, "Instead of restoring from backup, initialize an empty database with the provided init_db_sql_file and upload a backup of that for the shard, if the shard has no backups yet. This can be used to seed a brand new shard with an initial, empty backup. If any backups already exist for the shard, this will be considered a successful no-op. This can only be done before the shard exists in topology (i.e. before any tablets are deployed).")
	Main.Flags().BoolVar(&allowFirstBackup, "allow_first_backup", allowFirstBackup, "Allow this job to take the first backup of an existing shard.")
	Main.Flags().BoolVar(&restartBeforeBackup, "restart_before_backup", restartBeforeBackup, "Perform a mysqld clean/full restart after applying binlogs, but before taking the backup. Only makes sense to work around xtrabackup bugs.")
	Main.Flags().BoolVar(&upgradeSafe, "upgrade-safe", upgradeSafe, "Whether to use innodb_fast_shutdown=0 for the backup so it is safe to use for MySQL upgrades.")

	// vttablet-like flags
	utils.SetFlagStringVar(Main.Flags(), &initDbNameOverride, "init-db-name-override", initDbNameOverride, "(init parameter) override the name of the db used by vttablet")
	utils.SetFlagStringVar(Main.Flags(), &initKeyspace, "init-keyspace", initKeyspace, "(init parameter) keyspace to use for this tablet")
	utils.SetFlagStringVar(Main.Flags(), &initShard, "init-shard", initShard, "(init parameter) shard to use for this tablet")
	Main.Flags().IntVar(&concurrency, "concurrency", concurrency, "(init restore parameter) how many concurrent files to restore at once")
	Main.Flags().StringVar(&incrementalFromPos, "incremental_from_pos", incrementalFromPos, "Position, or name of backup from which to create an incremental backup. Default: empty. If given, then this backup becomes an incremental backup from given position or given backup. If value is 'auto', this backup will be taken from the last successful backup position.")

	// mysqlctld-like flags
	utils.SetFlagIntVar(Main.Flags(), &mysqlPort, "mysql-port", mysqlPort, "MySQL port")
	utils.SetFlagStringVar(Main.Flags(), &mysqlSocket, "mysql-socket", mysqlSocket, "Path to the mysqld socket file")
	Main.Flags().DurationVar(&mysqlTimeout, "mysql_timeout", mysqlTimeout, "how long to wait for mysqld startup")
	Main.Flags().DurationVar(&mysqlShutdownTimeout, "mysql-shutdown-timeout", mysqlShutdownTimeout, "how long to wait for mysqld shutdown")
	Main.Flags().StringVar(&initDBSQLFile, "init_db_sql_file", initDBSQLFile, "path to .sql file to run after mysql_install_db")
	Main.Flags().BoolVar(&detachedMode, "detach", detachedMode, "detached mode - run backups detached from the terminal")
	Main.Flags().DurationVar(&keepAliveTimeout, "keep-alive-timeout", keepAliveTimeout, "Wait until timeout elapses after a successful backup before shutting down.")
	Main.Flags().BoolVar(&disableRedoLog, "disable-redo-log", disableRedoLog, "Disable InnoDB redo log during replication-from-primary phase of backup.")

	acl.RegisterFlags(Main.Flags())

	collationEnv = collations.NewEnvironment(servenv.MySQLServerVersion())
}

func run(cc *cobra.Command, args []string) error {
	servenv.Init()

	ctx, cancel := context.WithCancel(cc.Context())
	servenv.OnClose(func() {
		cancel()
	})

	defer func() {
		servenv.ExitChan <- syscall.SIGTERM
		<-ctx.Done()
	}()

	go servenv.RunDefault()
	// Some stats plugins use OnRun to initialize. Wait for them to finish
	// initializing before continuing, so we don't lose any stats.
	if err := stats.AwaitBackend(ctx); err != nil {
		return fmt.Errorf("failed to await stats backend: %w", err)
	}

	if detachedMode {
		// this method will call os.Exit and kill this process
		cmd.DetachFromTerminalAndExit()
	}

	defer logutil.Flush()

	if minRetentionCount < 1 {
		log.Errorf("min_retention_count must be at least 1 to allow restores to succeed")
		exit.Return(1)
	}

	// Open connection backup storage.
	backupStorage, err := backupstorage.GetBackupStorage()
	if err != nil {
		return fmt.Errorf("Can't get backup storage: %w", err)
	}
	defer backupStorage.Close()
	// Open connection to topology server.
	topoServer := topo.Open()
	defer topoServer.Close()

	// Initialize stats.
	for _, phaseName := range phaseNames {
		phase.Set(phaseName, int64(0))
	}
	for phaseName, statuses := range phaseStatuses {
		for _, status := range statuses {
			phaseStatus.Set([]string{phaseName, status}, 0)
		}
	}

	// Try to take a backup, if it's been long enough since the last one.
	// Skip pruning if backup wasn't fully successful. We don't want to be
	// deleting things if the backup process is not healthy.
	backupDir := mysqlctl.GetBackupDir(initKeyspace, initShard)
	doBackup, err := shouldBackup(ctx, topoServer, backupStorage, backupDir)
	if err != nil {
		return fmt.Errorf("Can't take backup: %w", err)
	}
	if doBackup {
		if err := takeBackup(ctx, cc.Context(), topoServer, backupStorage); err != nil {
			return fmt.Errorf("Failed to take backup: %w", err)
		}
	}

	// Prune old backups.
	if err := pruneBackups(ctx, backupStorage, backupDir); err != nil {
		return fmt.Errorf("Couldn't prune old backups: %w", err)
	}

	if keepAliveTimeout > 0 {
		log.Infof("Backup was successful, waiting %s before exiting (or until context expires).", keepAliveTimeout)
		select {
		case <-time.After(keepAliveTimeout):
		case <-ctx.Done():
		}
	}
	log.Info("Exiting.")

	return nil
}

func takeBackup(ctx, backgroundCtx context.Context, topoServer *topo.Server, backupStorage backupstorage.BackupStorage) error {
	// This is an imaginary tablet alias. The value doesn't matter for anything,
	// except that we generate a random UID to ensure the target backup
	// directory is unique if multiple vtbackup instances are launched for the
	// same shard, at exactly the same second, pointed at the same backup
	// storage location.
	bigN, err := rand.Int(rand.Reader, big.NewInt(math.MaxUint32))
	if err != nil {
		return fmt.Errorf("can't generate random tablet UID: %v", err)
	}
	tabletAlias := &topodatapb.TabletAlias{
		Cell: "vtbackup",
		Uid:  uint32(bigN.Uint64()),
	}

	// Clean up our temporary data dir if we exit for any reason, to make sure
	// every invocation of vtbackup starts with a clean slate, and it does not
	// accumulate garbage (and run out of disk space) if it's restarted.
	tabletDir := mysqlctl.TabletDir(tabletAlias.Uid)
	defer func() {
		log.Infof("Removing temporary tablet directory: %v", tabletDir)
		if err := os.RemoveAll(tabletDir); err != nil {
			log.Warningf("Failed to remove temporary tablet directory: %v", err)
		}
	}()

	// Start up mysqld as if we are mysqlctld provisioning a fresh tablet.
	mysqld, mycnf, err := mysqlctl.CreateMysqldAndMycnf(tabletAlias.Uid, mysqlSocket, mysqlPort, collationEnv)
	if err != nil {
		return fmt.Errorf("failed to initialize mysql config: %v", err)
	}
	ctx, cancelCtx := context.WithCancel(ctx)
	backgroundCtx, cancelBackgroundCtx := context.WithCancel(backgroundCtx)
	defer func() {
		cancelCtx()
		cancelBackgroundCtx()
	}()
	mysqld.OnTerm(func() {
		log.Warning("Cancelling vtbackup as MySQL has terminated")
		cancelCtx()
		cancelBackgroundCtx()
	})

	initCtx, initCancel := context.WithTimeout(ctx, mysqlTimeout)
	defer initCancel()
	initMysqldAt := time.Now()
	if err := mysqld.Init(initCtx, mycnf, initDBSQLFile); err != nil {
		return fmt.Errorf("failed to initialize mysql data dir and start mysqld: %v", err)
	}
	deprecatedDurationByPhase.Set("InitMySQLd", int64(time.Since(initMysqldAt).Seconds()))
	// Shut down mysqld when we're done.
	defer func() {
		// Be careful use the background context, not the init one, because we don't want to
		// skip shutdown just because we timed out waiting for init.
		mysqlShutdownCtx, mysqlShutdownCancel := context.WithTimeout(backgroundCtx, mysqlShutdownTimeout+10*time.Second)
		defer mysqlShutdownCancel()
		if err := mysqld.Shutdown(mysqlShutdownCtx, mycnf, false, mysqlShutdownTimeout); err != nil {
			log.Errorf("failed to shutdown mysqld: %v", err)
		}
	}()

	extraEnv := map[string]string{
		"TABLET_ALIAS": topoproto.TabletAliasString(tabletAlias),
	}
	dbName := initDbNameOverride
	if dbName == "" {
		dbName = fmt.Sprintf("vt_%s", initKeyspace)
	}

	backupParams := mysqlctl.BackupParams{
		Cnf:                  mycnf,
		Mysqld:               mysqld,
		Logger:               logutil.NewConsoleLogger(),
		Concurrency:          concurrency,
		IncrementalFromPos:   incrementalFromPos,
		HookExtraEnv:         extraEnv,
		TopoServer:           topoServer,
		Keyspace:             initKeyspace,
		Shard:                initShard,
		TabletAlias:          topoproto.TabletAliasString(tabletAlias),
		Stats:                backupstats.BackupStats(),
		UpgradeSafe:          upgradeSafe,
		MysqlShutdownTimeout: mysqlShutdownTimeout,
	}
	// In initial_backup mode, just take a backup of this empty database.
	if initialBackup {
		// Take a backup of this empty DB without restoring anything.
		// First, initialize it the way InitShardPrimary would, so this backup
		// produces a result that can be used to skip InitShardPrimary entirely.
		// This involves resetting replication (to erase any history) and then
		// creating the main database and some Vitess system tables.
		if err := mysqld.ResetReplication(ctx); err != nil {
			return fmt.Errorf("can't reset replication: %v", err)
		}
		// We need to switch off super_read_only before we create the database.
		resetFunc, err := mysqld.SetSuperReadOnly(ctx, false)
		if err != nil {
			return fmt.Errorf("failed to disable super_read_only during backup: %v", err)
		}
		if resetFunc != nil {
			defer func() {
				err := resetFunc()
				if err != nil {
					log.Error("Failed to set super_read_only back to its original value during backup")
				}
			}()
		}
		cmd := mysqlctl.GenerateInitialBinlogEntry()
		if err := mysqld.ExecuteSuperQueryList(ctx, []string{cmd}); err != nil {
			return err
		}

		backupParams.BackupTime = time.Now()
		// Now we're ready to take the backup.
		phase.Set(phaseNameInitialBackup, int64(1))
		defer phase.Set(phaseNameInitialBackup, int64(0))
		if err := mysqlctl.Backup(ctx, backupParams); err != nil {
			return fmt.Errorf("backup failed: %v", err)
		}
		deprecatedDurationByPhase.Set("InitialBackup", int64(time.Since(backupParams.BackupTime).Seconds()))
		log.Info("Initial backup successful.")
		phase.Set(phaseNameInitialBackup, int64(0))
		return nil
	}

	phase.Set(phaseNameRestoreLastBackup, int64(1))
	defer phase.Set(phaseNameRestoreLastBackup, int64(0))
	backupDir := mysqlctl.GetBackupDir(initKeyspace, initShard)
	log.Infof("Restoring latest backup from directory %v", backupDir)
	restoreAt := time.Now()
	params := mysqlctl.RestoreParams{
		Cnf:                  mycnf,
		Mysqld:               mysqld,
		Logger:               logutil.NewConsoleLogger(),
		Concurrency:          concurrency,
		HookExtraEnv:         extraEnv,
		DeleteBeforeRestore:  true,
		DbName:               dbName,
		Keyspace:             initKeyspace,
		Shard:                initShard,
		Stats:                backupstats.RestoreStats(),
		MysqlShutdownTimeout: mysqlShutdownTimeout,
	}
	backupManifest, err := mysqlctl.Restore(ctx, params)
	var restorePos replication.Position
	switch err {
	case nil:
		// if err is nil, we expect backupManifest to be non-nil
		restorePos = backupManifest.Position
		log.Infof("Successfully restored from backup at replication position %v", restorePos)
	case mysqlctl.ErrNoBackup:
		// There is no backup found, but we may be taking the initial backup of a shard
		if !allowFirstBackup {
			return fmt.Errorf("no backup found; not starting up empty since --initial_backup flag was not enabled")
		}
		restorePos = replication.Position{}
	default:
		return fmt.Errorf("can't restore from backup: %v", err)
	}
	deprecatedDurationByPhase.Set("RestoreLastBackup", int64(time.Since(restoreAt).Seconds()))
	phase.Set(phaseNameRestoreLastBackup, int64(0))

	// As of MySQL 8.0.21, you can disable redo logging using the ALTER INSTANCE
	// DISABLE INNODB REDO_LOG statement. This functionality is intended for
	// loading data into a new MySQL instance. Disabling redo logging speeds up
	// data loading by avoiding redo log writes and doublewrite buffering.
	disabledRedoLog := false
	if disableRedoLog {
		if err := mysqld.DisableRedoLog(ctx); err != nil {
			log.Warningf("Error disabling redo logging: %v", err)
		} else {
			disabledRedoLog = true
		}
	}

	// We have restored a backup. Now start replication.
	if err := resetReplication(ctx, restorePos, mysqld); err != nil {
		return fmt.Errorf("error resetting replication: %v", err)
	}
	if err := startReplication(ctx, mysqld, topoServer); err != nil {
		return fmt.Errorf("error starting replication: %v", err)
	}

	log.Info("get the current primary replication position, and wait until we catch up")
	// Get the current primary replication position, and wait until we catch up
	// to that point. We do this instead of looking at ReplicationLag
	// because that value can
	// sometimes lie and tell you there's 0 lag when actually replication is
	// stopped. Also, if replication is making progress but is too slow to ever
	// catch up to live changes, we'd rather take a backup of something rather
	// than timing out.
	tmc := tmclient.NewTabletManagerClient()
	// Keep retrying if we can't contact the primary. The primary might be
	// changing, moving, or down temporarily.
	var primaryPos replication.Position
	err = retryOnError(ctx, func() error {
		// Add a per-operation timeout so we re-read topo if the primary is unreachable.
		opCtx, optCancel := context.WithTimeout(ctx, operationTimeout)
		defer optCancel()
		pos, err := getPrimaryPosition(opCtx, tmc, topoServer)
		if err != nil {
			return fmt.Errorf("can't get the primary replication position: %v", err)
		}
		primaryPos = pos
		return nil
	})
	if err != nil {
		return fmt.Errorf("can't get the primary replication position after all retries: %v", err)
	}

	log.Infof("takeBackup: primary position is: %s", primaryPos.String())

	// Remember the time when we fetched the primary position, not when we caught
	// up to it, so the timestamp on our backup is honest (assuming we make it
	// to the goal position).
	backupParams.BackupTime = time.Now()

	// Wait for replication to catch up.
	phase.Set(phaseNameCatchupReplication, int64(1))
	defer phase.Set(phaseNameCatchupReplication, int64(0))

	var (
		lastStatus replication.ReplicationStatus
		status     replication.ReplicationStatus
		statusErr  error

		waitStartTime = time.Now()
	)

	lastErr := vterrors.NewLastError("replication catch up", timeoutWaitingForReplicationStatus)
	for {
		if !lastErr.ShouldRetry() {
			return fmt.Errorf("timeout waiting for replication status after %.0f seconds", timeoutWaitingForReplicationStatus.Seconds())
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("error in replication catch up: %v", ctx.Err())
		case <-time.After(time.Second):
		}

		lastStatus = status
		status, statusErr = mysqld.ReplicationStatus(ctx)
		if statusErr != nil {
			lastErr.Record(statusErr)
			log.Warningf("Error getting replication status: %v", statusErr)
			continue
		}
		if status.Position.AtLeast(primaryPos) {
			// We're caught up on replication to at least the point the primary
			// was at when this vtbackup run started.
			log.Infof("Replication caught up to %v after %v", status.Position, time.Since(waitStartTime))
			deprecatedDurationByPhase.Set("CatchUpReplication", int64(time.Since(waitStartTime).Seconds()))
			break
		}
		if !lastStatus.Position.IsZero() {
			if status.Position.Equal(lastStatus.Position) {
				phaseStatus.Set([]string{phaseNameCatchupReplication, phaseStatusCatchupReplicationStalled}, 1)
			} else {
				phaseStatus.Set([]string{phaseNameCatchupReplication, phaseStatusCatchupReplicationStalled}, 0)
			}
		}
		if !status.Healthy() {
			errStr := "Replication has stopped before backup could be taken. Trying to restart replication."
			log.Warning(errStr)
			lastErr.Record(errors.New(strings.ToLower(errStr)))

			phaseStatus.Set([]string{phaseNameCatchupReplication, phaseStatusCatchupReplicationStopped}, 1)
			if err := startReplication(ctx, mysqld, topoServer); err != nil {
				log.Warningf("Failed to restart replication: %v", err)
			}
		} else {
			phaseStatus.Set([]string{phaseNameCatchupReplication, phaseStatusCatchupReplicationStopped}, 0)
		}
	}
	phase.Set(phaseNameCatchupReplication, int64(0))

	// Stop replication and see where we are.
	if err := mysqld.StopReplication(ctx, nil); err != nil {
		return fmt.Errorf("can't stop replication: %v", err)
	}

	// Did we make any progress?
	status, statusErr = mysqld.ReplicationStatus(ctx)
	if statusErr != nil {
		return fmt.Errorf("can't get replication status: %v", err)
	}
	log.Infof("Replication caught up to %v", status.Position)
	if !status.Position.AtLeast(primaryPos) && status.Position.Equal(restorePos) {
		return fmt.Errorf("not taking backup: replication did not make any progress from restore point: %v", restorePos)
	}
	phaseStatus.Set([]string{phaseNameCatchupReplication, phaseStatusCatchupReplicationStalled}, 0)
	phaseStatus.Set([]string{phaseNameCatchupReplication, phaseStatusCatchupReplicationStopped}, 0)

	// Re-enable redo logging.
	if disabledRedoLog {
		if err := mysqld.EnableRedoLog(ctx); err != nil {
			return fmt.Errorf("failed to re-enable redo log: %v", err)
		}
	}

	if restartBeforeBackup {
		restartAt := time.Now()
		log.Info("Proceeding with clean MySQL shutdown and startup to flush all buffers.")
		// Prep for full/clean shutdown (not typically the default)
		if err := mysqld.ExecuteSuperQuery(ctx, "SET GLOBAL innodb_fast_shutdown=0"); err != nil {
			return fmt.Errorf("Could not prep for full shutdown: %v", err)
		}
		// Shutdown, waiting for it to finish
		if err := mysqld.Shutdown(ctx, mycnf, true, mysqlShutdownTimeout); err != nil {
			return fmt.Errorf("Something went wrong during full MySQL shutdown: %v", err)
		}
		// Start MySQL, waiting for it to come up
		if err := mysqld.Start(ctx, mycnf); err != nil {
			return fmt.Errorf("Could not start MySQL after full shutdown: %v", err)
		}
		deprecatedDurationByPhase.Set("RestartBeforeBackup", int64(time.Since(restartAt).Seconds()))
	}

	// Now we can take a new backup.
	backupAt := time.Now()
	phase.Set(phaseNameTakeNewBackup, int64(1))
	defer phase.Set(phaseNameTakeNewBackup, int64(0))
	if err := mysqlctl.Backup(ctx, backupParams); err != nil {
		return fmt.Errorf("error taking backup: %v", err)
	}
	deprecatedDurationByPhase.Set("TakeNewBackup", int64(time.Since(backupAt).Seconds()))
	phase.Set(phaseNameTakeNewBackup, int64(0))

	// Return a non-zero exit code if we didn't meet the replication position
	// goal, even though we took a backup that pushes the high-water mark up.
	if !status.Position.AtLeast(primaryPos) {
		return fmt.Errorf("replication caught up to %v but didn't make it to the goal of %v; a backup was taken anyway to save partial progress, but the operation should still be retried since not all expected data is backed up", status.Position, primaryPos)
	}
	log.Info("Backup successful.")
	return nil
}

func resetReplication(ctx context.Context, pos replication.Position, mysqld mysqlctl.MysqlDaemon) error {
	if err := mysqld.StopReplication(ctx, nil); err != nil {
		return vterrors.Wrap(err, "failed to stop replication")
	}
	if err := mysqld.ResetReplicationParameters(ctx); err != nil {
		return vterrors.Wrap(err, "failed to reset replication")
	}

	// Check if we have a position to resume from, if not reset to the beginning of time
	if !pos.IsZero() {
		// Set the position at which to resume from the replication source.
		if err := mysqld.SetReplicationPosition(ctx, pos); err != nil {
			return vterrors.Wrap(err, "failed to set replica position")
		}
	} else {
		if err := mysqld.ResetReplication(ctx); err != nil {
			return vterrors.Wrap(err, "failed to reset replication")
		}
	}
	return nil
}

func startReplication(ctx context.Context, mysqld mysqlctl.MysqlDaemon, topoServer *topo.Server) error {
	si, err := topoServer.GetShard(ctx, initKeyspace, initShard)
	if err != nil {
		return vterrors.Wrap(err, "can't read shard")
	}
	if topoproto.TabletAliasIsZero(si.PrimaryAlias) {
		// Normal tablets will sit around waiting to be reparented in this case.
		// Since vtbackup is a batch job, we just have to fail.
		return fmt.Errorf("can't start replication after restore: shard %v/%v has no primary", initKeyspace, initShard)
	}
	// TODO(enisoc): Support replicating from another replica, preferably in the
	//   same cell, preferably rdonly, to reduce load on the primary.
	ti, err := topoServer.GetTablet(ctx, si.PrimaryAlias)
	if err != nil {
		return vterrors.Wrapf(err, "Cannot read primary tablet %v", si.PrimaryAlias)
	}

	// Stop replication (in case we're restarting), set replication source, and start replication.
	if err := mysqld.SetReplicationSource(ctx, ti.Tablet.MysqlHostname, ti.Tablet.MysqlPort, 0, true, true); err != nil {
		return vterrors.Wrap(err, "MysqlDaemon.SetReplicationSource failed")
	}
	return nil
}

func getPrimaryPosition(ctx context.Context, tmc tmclient.TabletManagerClient, ts *topo.Server) (replication.Position, error) {
	si, err := ts.GetShard(ctx, initKeyspace, initShard)
	if err != nil {
		return replication.Position{}, vterrors.Wrap(err, "can't read shard")
	}
	if topoproto.TabletAliasIsZero(si.PrimaryAlias) {
		// Normal tablets will sit around waiting to be reparented in this case.
		// Since vtbackup is a batch job, we just have to fail.
		return replication.Position{}, fmt.Errorf("shard %v/%v has no primary", initKeyspace, initShard)
	}
	ti, err := ts.GetTablet(ctx, si.PrimaryAlias)
	if err != nil {
		return replication.Position{}, fmt.Errorf("can't get primary tablet record %v: %v", topoproto.TabletAliasString(si.PrimaryAlias), err)
	}
	posStr, err := tmc.PrimaryPosition(ctx, ti.Tablet)
	if err != nil {
		return replication.Position{}, fmt.Errorf("can't get primary replication position: %v", err)
	}
	pos, err := replication.DecodePosition(posStr)
	if err != nil {
		return replication.Position{}, fmt.Errorf("can't decode primary replication position %q: %v", posStr, err)
	}
	return pos, nil
}

// retryOnError keeps calling the given function until it succeeds, or the given
// Context is done. It waits an exponentially increasing amount of time between
// retries to avoid hot-looping. The only time this returns an error is if the
// Context is cancelled.
func retryOnError(ctx context.Context, fn func() error) error {
	waitTime := 1 * time.Second

	for {
		err := fn()
		if err == nil {
			return nil
		}
		log.Errorf("Waiting %v to retry after error: %v", waitTime, err)

		select {
		case <-ctx.Done():
			log.Errorf("Not retrying after error: %v", ctx.Err())
			return ctx.Err()
		case <-time.After(waitTime):
			waitTime *= 2
		}
	}
}

func pruneBackups(ctx context.Context, backupStorage backupstorage.BackupStorage, backupDir string) error {
	if minRetentionTime == 0 {
		log.Info("Pruning of old backups is disabled.")
		return nil
	}
	backups, err := backupStorage.ListBackups(ctx, backupDir)
	if err != nil {
		return fmt.Errorf("can't list backups: %v", err)
	}
	numBackups := len(backups)
	if numBackups <= minRetentionCount {
		log.Infof("Found %v backups. Not pruning any since this is within the min_retention_count of %v.", numBackups, minRetentionCount)
		return nil
	}
	// We have more than the minimum retention count, so we could afford to
	// prune some. See if any are beyond the minimum retention time.
	// ListBackups returns them sorted by oldest first.
	for _, backup := range backups {
		backupTime, err := parseBackupTime(backup.Name())
		if err != nil {
			return err
		}
		if time.Since(backupTime) < minRetentionTime {
			// The oldest remaining backup is not old enough to prune.
			log.Infof("Oldest backup taken at %v has not reached min_retention_time of %v. Nothing left to prune.", backupTime, minRetentionTime)
			break
		}
		// Remove the backup.
		log.Infof("Removing old backup %v from %v, since it's older than min_retention_time of %v", backup.Name(), backupDir, minRetentionTime)
		if err := backupStorage.RemoveBackup(ctx, backupDir, backup.Name()); err != nil {
			return fmt.Errorf("couldn't remove backup %v from %v: %v", backup.Name(), backupDir, err)
		}
		// We successfully removed one backup. Can we afford to prune any more?
		numBackups--
		if numBackups == minRetentionCount {
			log.Infof("Successfully pruned backup count to min_retention_count of %v.", minRetentionCount)
			break
		}
	}
	return nil
}

func parseBackupTime(name string) (time.Time, error) {
	// Backup names are formatted as "date.time.tablet-alias".
	parts := strings.Split(name, ".")
	if len(parts) != 3 {
		return time.Time{}, fmt.Errorf("backup name not in expected format (date.time.tablet-alias): %v", name)
	}
	backupTime, err := time.Parse(mysqlctl.BackupTimestampFormat, fmt.Sprintf("%s.%s", parts[0], parts[1]))
	if err != nil {
		return time.Time{}, fmt.Errorf("can't parse timestamp from backup %q: %v", name, err)
	}
	return backupTime, nil
}

func shouldBackup(ctx context.Context, topoServer *topo.Server, backupStorage backupstorage.BackupStorage, backupDir string) (bool, error) {
	// Look for the most recent, complete backup.
	backups, err := backupStorage.ListBackups(ctx, backupDir)
	if err != nil {
		return false, fmt.Errorf("can't list backups: %v", err)
	}
	lastBackup := lastCompleteBackup(ctx, backups)

	// Check preconditions for initial_backup mode.
	if initialBackup {
		// Check if any backups for the shard already exist in this backup storage location.
		if lastBackup != nil {
			log.Infof("At least one complete backup already exists, so there's no need to seed an empty backup. Doing nothing.")
			return false, nil
		}

		// Check whether the shard exists.
		_, shardErr := topoServer.GetShard(ctx, initKeyspace, initShard)
		switch {
		case shardErr == nil:
			// If the shard exists, we should make sure none of the tablets are
			// already in a serving state, because then they might have data
			// that conflicts with the initial backup we're about to take.
			tablets, err := topoServer.GetTabletMapForShard(ctx, initKeyspace, initShard)
			if err != nil {
				// We don't know for sure whether any tablets are serving,
				// so it's not safe to continue.
				return false, fmt.Errorf("failed to check whether shard %v/%v has serving tablets before doing initial backup: %v", initKeyspace, initShard, err)
			}
			for tabletAlias, tablet := range tablets {
				// Check if any tablet has its type set to one of the serving types.
				// If so, it's too late to do an initial backup.
				if tablet.IsInServingGraph() {
					return false, fmt.Errorf("refusing to upload initial backup of empty database: the shard %v/%v already has at least one tablet that may be serving (%v); you must take a backup from a live tablet instead", initKeyspace, initShard, tabletAlias)
				}
			}
			log.Infof("Shard %v/%v exists but has no serving tablets.", initKeyspace, initShard)
		case topo.IsErrType(shardErr, topo.NoNode):
			// The shard doesn't exist, so we know no tablets are running.
			log.Infof("Shard %v/%v doesn't exist; assuming it has no serving tablets.", initKeyspace, initShard)
		default:
			// If we encounter any other error, we don't know for sure whether
			// the shard exists, so it's not safe to continue.
			return false, fmt.Errorf("failed to check whether shard %v/%v exists before doing initial backup: %v", initKeyspace, initShard, err)
		}

		log.Infof("Shard %v/%v has no existing backups. Creating initial backup.", initKeyspace, initShard)
		return true, nil
	}

	// We need at least one backup so we can restore first, unless the user explicitly says we don't
	if len(backups) == 0 && !allowFirstBackup {
		return false, fmt.Errorf("no existing backups to restore from; backup is not possible since --initial_backup flag was not enabled")
	}
	if lastBackup == nil {
		if allowFirstBackup {
			// There's no complete backup, but we were told to take one from scratch anyway.
			return true, nil
		}
		return false, fmt.Errorf("no complete backups to restore from; backup is not possible since --initial_backup flag was not enabled")
	}

	// Has it been long enough since the last complete backup to need a new one?
	if minBackupInterval == 0 {
		// No minimum interval is set, so always backup.
		return true, nil
	}
	lastBackupTime, err := parseBackupTime(lastBackup.Name())
	if err != nil {
		return false, fmt.Errorf("can't check last backup time: %v", err)
	}
	if elapsedTime := time.Since(lastBackupTime); elapsedTime < minBackupInterval {
		// It hasn't been long enough yet.
		log.Infof("Skipping backup since only %v has elapsed since the last backup at %v, which is less than the min_backup_interval of %v.", elapsedTime, lastBackupTime, minBackupInterval)
		return false, nil
	}
	// It has been long enough.
	log.Infof("The last backup was taken at %v, which is older than the min_backup_interval of %v.", lastBackupTime, minBackupInterval)
	return true, nil
}

func lastCompleteBackup(ctx context.Context, backups []backupstorage.BackupHandle) backupstorage.BackupHandle {
	if len(backups) == 0 {
		return nil
	}

	// Backups are sorted in ascending order by start time. Start at the end.
	for i := len(backups) - 1; i >= 0; i-- {
		// Check if this backup is complete by looking for the MANIFEST file,
		// which is written at the end after all files are uploaded.
		backup := backups[i]
		if err := checkBackupComplete(ctx, backup); err != nil {
			log.Warningf("Ignoring backup %v because it's incomplete: %v", backup.Name(), err)
			continue
		}
		return backup
	}

	return nil
}

func checkBackupComplete(ctx context.Context, backup backupstorage.BackupHandle) error {
	manifest, err := mysqlctl.GetBackupManifest(ctx, backup)
	if err != nil {
		return fmt.Errorf("can't get backup MANIFEST: %v", err)
	}

	log.Infof("Found complete backup %v taken at position %v", backup.Name(), manifest.Position.String())
	return nil
}
