// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// This file pins ApplyBootstrapDDL — the shared DDL-convergence body of
// the boot's bootstrap_schema step and the `chinit schema-bootstrap`
// timer unit. The a reader exhausted the boot's
// retries while the keeper rolled in the same instance refresh and
// stayed schemaless for HOURS because nothing re-ran the DDL after the
// boot gave up. These specs pin the contract the timer's self-heal
// depends on: the pass is idempotent, failures COUNT, and the Code 253
// stale-znode path drops this node's OWN registration before retrying.

var _ = Describe("ApplyBootstrapDDL", func() {
	var (
		dir          string
		queries      []string
		failOn       map[string]string
		restoreExec  func()
		restorePaths func()
	)

	BeforeEach(func() {
		dir = filepath.Join(os.TempDir(), "chinit-apply-ddl-test")
		Expect(os.MkdirAll(dir, 0755)).To(Succeed())

		Expect(os.WriteFile(filepath.Join(dir, "bootstrap-ddl.sql"), []byte(
			`-- Bundle DDL
CREATE TABLE IF NOT EXISTS events (id String) ENGINE = ReplicatedMergeTree('/clickhouse/tables/01/events', '{replica}');
CREATE TABLE IF NOT EXISTS staging_synth (id String) ENGINE = S3Queue('s3://x/staging/', 'AWS_ACCESS_KEY_ID', 'AWS_SECRET_ACCESS_KEY', 'CSV');
`), 0644)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(dir, "admin-password"), []byte("sekrit\n"), 0600)).To(Succeed())

		queries = nil
		failOn = map[string]string{}
		execClientFn = func(_ context.Context, _, query string) (string, error) {
			queries = append(queries, query)
			if msg, ok := failOn[query]; ok {
				return msg, &errString{msg}
			}
			return "", nil
		}
		restoreExec = func() { execClientFn = nil }
		DeferCleanup(restoreExec)

		ddl := bootstrapDDLPath
		pw := bootstrapAdminPasswordPath
		restorePaths = func() { bootstrapDDLPath, bootstrapAdminPasswordPath = ddl, pw }
		bootstrapDDLPath = func() string { return filepath.Join(dir, "bootstrap-ddl.sql") }
		bootstrapAdminPasswordPath = func() string { return filepath.Join(dir, "admin-password") }
		DeferCleanup(restorePaths)
	})

	It("applies every CREATE and reports zero failures on a clean pass", func() {
		result, err := ApplyBootstrapDDL(context.Background(), "writer-0", "writer")
		Expect(err).NotTo(HaveOccurred())
		Expect(result.Created).To(Equal(2))
		Expect(result.Failed).To(Equal(0))
		// Both statements reached the client; the {replica} macro form
		// is passed through for server-side substitution. The S3Queue
		// CREATE is preceded by its drift probe (keeper_path/URL —
		// every S3Queue declares a URL, so the probe always runs).
		Expect(queries).To(HaveLen(3))
		Expect(queries[0]).To(ContainSubstring("/clickhouse/tables/01/events"))
		Expect(queries[0]).To(ContainSubstring("'{replica}'"))
		Expect(queries[1]).To(ContainSubstring("SELECT engine_full FROM system.tables"))
		Expect(queries[1]).To(ContainSubstring("name = 'staging_synth'"))
		Expect(queries[2]).To(ContainSubstring("S3Queue('s3://x/staging/'"))
	})

	It("counts failures so the caller surfaces a retryable error", func() {
		failOn["CREATE TABLE IF NOT EXISTS events (id String) ENGINE = ReplicatedMergeTree('/clickhouse/tables/01/events', '{replica}')"] =
			"Code: 999. ZooKeeper connection refused"
		result, err := ApplyBootstrapDDL(context.Background(), "writer-0", "writer")
		Expect(err).NotTo(HaveOccurred(), "the pass itself errors only on missing credentials; statement failures COUNT")
		Expect(result.Failed).To(Equal(1))
		Expect(result.Created).To(Equal(1))
	})

	It("drops the node's OWN stale znode on Code 253 and retries the CREATE", func() {
		const stmt = "CREATE TABLE IF NOT EXISTS events (id String) ENGINE = ReplicatedMergeTree('/clickhouse/tables/01/events', '{replica}')"
		failOnce := true
		execClientFn = func(_ context.Context, _, query string) (string, error) {
			queries = append(queries, query)
			if query == stmt && failOnce {
				failOnce = false
				return "Code: 253. Replica /clickhouse/tables/01/events already exists", &errString{"Code: 253"}
			}
			return "", nil
		}
		result, err := ApplyBootstrapDDL(context.Background(), "writer-0", "writer")
		Expect(err).NotTo(HaveOccurred())
		Expect(result.Created).To(Equal(2))
		Expect(result.Failed).To(Equal(0))
		// The drop names THIS node's replica (hostname substitution of
		// the {replica} macro) and its OWN zk path.
		Expect(queries).To(ContainElement("SYSTEM DROP REPLICA 'writer-0' FROM ZKPATH '/clickhouse/tables/01/events'"))
		// The retried CREATE follows the drop.
		Expect(queries).To(ContainElement(stmt))
	})

	It("substitutes the {shard} macro in the stale-znode drop path", func() {
		// The the bundle DDL's engine args carry
		// CLICKHOUSE macros; SYSTEM DROP REPLICA takes a LITERAL path,
		// so an unresolved {shard} failed Code 305 and the Code 253
		// conflict stayed unhealed on every {shard}-macro CREATE.
		macros := filepath.Join(dir, "chop-generated-macros.xml")
		Expect(os.WriteFile(macros, []byte("<yandex>\n  <macros>\n    <shard>01</shard>\n  </macros>\n</yandex>\n"), 0644)).To(Succeed())
		smp := shardMacroPath
		shardMacroPath = func() string { return macros }
		DeferCleanup(func() { shardMacroPath = smp })

		Expect(os.WriteFile(bootstrapDDLPath(), []byte(
			"CREATE TABLE IF NOT EXISTS events (id String) ENGINE = ReplicatedMergeTree('/clickhouse/tables/{shard}/events', '{replica}');\n"), 0644)).To(Succeed())

		const stmt = "CREATE TABLE IF NOT EXISTS events (id String) ENGINE = ReplicatedMergeTree('/clickhouse/tables/{shard}/events', '{replica}')"
		failOnce := true
		execClientFn = func(_ context.Context, _, query string) (string, error) {
			queries = append(queries, query)
			if query == stmt && failOnce {
				failOnce = false
				return "Code: 253", &errString{"Code: 253"}
			}
			return "", nil
		}
		result, err := ApplyBootstrapDDL(context.Background(), "reader-0", "reader")
		Expect(err).NotTo(HaveOccurred())
		Expect(result.Created).To(Equal(1))
		Expect(queries).To(ContainElement("SYSTEM DROP REPLICA 'reader-0' FROM ZKPATH '/clickhouse/tables/01/events'"))
	})

	It("converges an S3Queue table onto the declared shared keeper path (drop + re-create)", func() {
		// The 2026-11-17 fan-out finding: without an explicit
		// keeper_path every node's CREATE invented its own UUID-derived
		// claim tree (5 writers, 5 trees, 4-of-5 claim races per file);
		// 26.8 refuses MODIFY SETTING keeper_path for unordered mode
		// (Code 344), so the convergence is a one-time DROP + CREATE.
		Expect(os.WriteFile(bootstrapDDLPath(), []byte(
			"CREATE TABLE IF NOT EXISTS staging_synth (id String) ENGINE = S3Queue('s3://x/staging/', 'NDJSON', 'auto') SETTINGS keeper_path = '/clickhouse/s3queue/staging_synth', mode = 'unordered';\n"), 0644)).To(Succeed())
		const createStmt = "CREATE TABLE IF NOT EXISTS staging_synth (id String) ENGINE = S3Queue('s3://x/staging/', 'NDJSON', 'auto') SETTINGS keeper_path = '/clickhouse/s3queue/staging_synth', mode = 'unordered'"
		const probeStmt = "SELECT engine_full FROM system.tables WHERE database = 'default' AND name = 'staging_synth'"
		execClientFn = func(_ context.Context, _, query string) (string, error) {
			queries = append(queries, query)
			// The live table exists with the OLD UUID-derived path.
			if query == probeStmt {
				return "S3Queue('s3://x/staging/', 'NDJSON', 'auto') SETTINGS keeper_path = '/clickhouse/s3queue/dea3107a/3165c671', mode = 'unordered'", nil
			}
			return "", nil
		}
		result, err := ApplyBootstrapDDL(context.Background(), "writer-0", "writer")
		Expect(err).NotTo(HaveOccurred())
		Expect(result.Failed).To(Equal(0))
		// The drop ran BEFORE the create, against the queue table only.
		Expect(queries).To(ContainElement("DROP TABLE IF EXISTS default.staging_synth"))
		Expect(queries).To(ContainElement(createStmt))
		Expect(queries[0]).To(Equal(probeStmt), "the drift probe precedes every other statement")
	})

	It("skips the S3Queue path convergence when the live path already matches", func() {
		Expect(os.WriteFile(bootstrapDDLPath(), []byte(
			"CREATE TABLE IF NOT EXISTS staging_synth (id String) ENGINE = S3Queue('s3://x/staging/', 'NDJSON', 'auto') SETTINGS keeper_path = '/clickhouse/s3queue/staging_synth', mode = 'unordered';\n"), 0644)).To(Succeed())
		const probeStmt = "SELECT engine_full FROM system.tables WHERE database = 'default' AND name = 'staging_synth'"
		execClientFn = func(_ context.Context, _, query string) (string, error) {
			queries = append(queries, query)
			if query == probeStmt {
				return "S3Queue('s3://x/staging/', 'NDJSON', 'auto') SETTINGS keeper_path = '/clickhouse/s3queue/staging_synth', mode = 'unordered'", nil
			}
			return "", nil
		}
		result, err := ApplyBootstrapDDL(context.Background(), "writer-0", "writer")
		Expect(err).NotTo(HaveOccurred())
		Expect(result.Failed).To(Equal(0))
		// No DROP — the declared path is already live.
		Expect(queries).NotTo(ContainElement(ContainSubstring("DROP TABLE")))
	})

	It("never drops a table when the CREATE declares no explicit keeper path and the URL matches", func() {
		// The convergence remains opt-in per DRIFT: a bundle without
		// the explicit keeper_path still probes the live table (the
		// URL is always declared — it is the first engine argument),
		// but a live table carrying the SAME URL and no path drift
		// must keep the no-op shape: never dropped, never re-created.
		Expect(os.WriteFile(bootstrapDDLPath(), []byte(
			"CREATE TABLE IF NOT EXISTS staging_synth (id String) ENGINE = S3Queue('s3://x/staging/', 'NDJSON', 'auto');\n"), 0644)).To(Succeed())
		const probeStmt = "SELECT engine_full FROM system.tables WHERE database = 'default' AND name = 'staging_synth'"
		execClientFn = func(_ context.Context, _, query string) (string, error) {
			queries = append(queries, query)
			if query == probeStmt {
				// A live table with the SAME URL (no path drift).
				return "S3Queue('s3://x/staging/', 'NDJSON', 'auto') SETTINGS mode = 'unordered'", nil
			}
			return "", nil
		}
		result, err := ApplyBootstrapDDL(context.Background(), "writer-0", "writer")
		Expect(err).NotTo(HaveOccurred())
		Expect(result.Created).To(Equal(1))
		for _, q := range queries {
			Expect(q).NotTo(ContainSubstring("DROP TABLE"))
		}
	})

	It("converges an S3Queue table whose live endpoint URL drifted (bucket rename through Terraform-rendered URLs)", func() {
		// The 2026-11-17 gateway-endpoint audit: bootstrap-ddl.sql's
		// S3Queue URL is rendered from storage_config variables, so a
		// bucket rename / region move changes the declared URL while
		// keeper_path stays put. CREATE IF NOT EXISTS would no-op
		// against the live table and writers would keep draining the
		// OLD bucket forever. The convergence must fire on URL drift
		// alone — the queue holds no data (after_processing=delete),
		// so the drop + re-create is lossless.
		Expect(os.WriteFile(bootstrapDDLPath(), []byte(
			"CREATE TABLE IF NOT EXISTS staging_synth (id String) ENGINE = S3Queue('https://new-raw-logs.s3.us-west-2.amazonaws.com/staging/*/*', 'NDJSON', 'auto') SETTINGS keeper_path = '/clickhouse/s3queue/staging_synth', mode = 'unordered';\n"), 0644)).To(Succeed())
		const createStmt = "CREATE TABLE IF NOT EXISTS staging_synth (id String) ENGINE = S3Queue('https://new-raw-logs.s3.us-west-2.amazonaws.com/staging/*/*', 'NDJSON', 'auto') SETTINGS keeper_path = '/clickhouse/s3queue/staging_synth', mode = 'unordered'"
		const probeStmt = "SELECT engine_full FROM system.tables WHERE database = 'default' AND name = 'staging_synth'"
		execClientFn = func(_ context.Context, _, query string) (string, error) {
			queries = append(queries, query)
			// The live table exists with the SAME keeper_path but the
			// OLD bucket URL (the pre-render literal).
			if query == probeStmt {
				return "S3Queue('https://old-raw-logs.s3.us-west-2.amazonaws.com/staging/*/*', 'NDJSON', 'auto') SETTINGS keeper_path = '/clickhouse/s3queue/staging_synth', mode = 'unordered'", nil
			}
			return "", nil
		}
		result, err := ApplyBootstrapDDL(context.Background(), "writer-0", "writer")
		Expect(err).NotTo(HaveOccurred())
		Expect(result.Failed).To(Equal(0))
		Expect(queries).To(ContainElement("DROP TABLE IF EXISTS default.staging_synth"))
		Expect(queries).To(ContainElement(createStmt))
	})

	It("does not falsely drop an already-converged S3Queue when clickhouse-client returns TabSeparated \\'-escaped quotes", func() {
		// Non-interactive clickhouse-client outputs TabSeparated by default,
		// escaping single quotes inside system.tables.engine_full as \'.
		Expect(os.WriteFile(bootstrapDDLPath(), []byte(
			"CREATE TABLE IF NOT EXISTS staging_synth (id String) ENGINE = S3Queue('https://example-raw-logs.s3.us-west-2.amazonaws.com/staging-synth/*/*', 'NDJSON', 'auto') SETTINGS keeper_path = '/clickhouse/s3queue/staging_synth', mode = 'unordered';\n"), 0644)).To(Succeed())
		const probeStmt = "SELECT engine_full FROM system.tables WHERE database = 'default' AND name = 'staging_synth'"
		execClientFn = func(_ context.Context, _, query string) (string, error) {
			queries = append(queries, query)
			if query == probeStmt {
				return `S3Queue(\'https://example-raw-logs.s3.us-west-2.amazonaws.com/staging-synth/*/*\', \'NDJSON\', \'auto\') SETTINGS keeper_path = \'/clickhouse/s3queue/staging_synth\', mode = \'unordered\'`, nil
			}
			return "", nil
		}
		result, err := ApplyBootstrapDDL(context.Background(), "writer-0", "writer")
		Expect(err).NotTo(HaveOccurred())
		Expect(result.Failed).To(Equal(0))
		Expect(queries).NotTo(ContainElement(ContainSubstring("DROP TABLE")))
	})

	It("no-ops loudly when the bundle carries no DDL (pre-0.2.14 AMI)", func() {
		Expect(os.Remove(filepath.Join(dir, "bootstrap-ddl.sql"))).To(Succeed())
		result, err := ApplyBootstrapDDL(context.Background(), "writer-0", "writer")
		Expect(err).NotTo(HaveOccurred())
		Expect(result.Created).To(Equal(0))
		Expect(result.Failed).To(Equal(0))
		Expect(queries).To(BeEmpty())
	})

	It("runs the reader-scoped DROP pair on readers (the reader-queue convergence)", func() {
		// The reader-OOM finding: readers materialized the ingest
		// pipeline under the old writer,reader tag, claimed files
		// through the shared keeper tree, and OOM'd on them (Code 241,
		// ~700 failed files/10min per 16 GiB reader). The bundle now
		// scopes the pipeline to writers and carries a reader/backup
		// DROP pair; this pass converges an existing reader clean on
		// its next schema-bootstrap tick.
		Expect(os.WriteFile(bootstrapDDLPath(), []byte(
			`--    chinit:roles=writer
CREATE TABLE IF NOT EXISTS staging_synth (id String) ENGINE = S3Queue('s3://x/staging/', 'NDJSON', 'auto');
--    chinit:roles=reader,backup
DROP TABLE IF EXISTS default.staging_to_events_synth;
DROP TABLE IF EXISTS default.staging_synth;
`), 0644)).To(Succeed())

		result, err := ApplyBootstrapDDL(context.Background(), "reader-0", "reader")
		Expect(err).NotTo(HaveOccurred())
		Expect(result.Failed).To(Equal(0))
		// The reader runs ONLY the DROP pair — the writer-tagged CREATE
		// is role-filtered before the server ever sees it.
		Expect(queries).To(HaveLen(2))
		Expect(queries).To(ContainElement("DROP TABLE IF EXISTS default.staging_to_events_synth"))
		Expect(queries).To(ContainElement("DROP TABLE IF EXISTS default.staging_synth"))
		for _, q := range queries {
			Expect(q).NotTo(ContainSubstring("CREATE TABLE IF NOT EXISTS staging_synth"))
		}
	})

	It("never runs the reader-scoped DROP pair on writers", func() {
		// The convergence's mirror: a writer MUST keep its queue. The
		// role filter drops the pair before exec, and the writer-tagged
		// CREATE is what runs.
		Expect(os.WriteFile(bootstrapDDLPath(), []byte(
			`--    chinit:roles=writer
CREATE TABLE IF NOT EXISTS staging_synth (id String) ENGINE = S3Queue('s3://x/staging/', 'NDJSON', 'auto');
--    chinit:roles=reader,backup
DROP TABLE IF EXISTS default.staging_to_events_synth;
DROP TABLE IF EXISTS default.staging_synth;
`), 0644)).To(Succeed())

		result, err := ApplyBootstrapDDL(context.Background(), "writer-0", "writer")
		Expect(err).NotTo(HaveOccurred())
		Expect(result.Failed).To(Equal(0))
		for _, q := range queries {
			Expect(q).NotTo(ContainSubstring("DROP TABLE"), "a writer's queue is never dropped by the convergence")
		}
		Expect(queries).To(ContainElement(ContainSubstring("CREATE TABLE IF NOT EXISTS staging_synth")))
	})

	It("ignores non-DDL statements that merely mention DROP (the verb is pinned to the IF EXISTS form)", func() {
		// A hand-edited bundle must never gain a destructive bare
		// DROP through the isDDLStatement gate: only the idempotent
		// DROP TABLE IF EXISTS form is executable DDL.
		Expect(os.WriteFile(bootstrapDDLPath(), []byte(
			`DROP TABLE default.events;
`), 0644)).To(Succeed())

		result, err := ApplyBootstrapDDL(context.Background(), "writer-0", "writer")
		Expect(err).NotTo(HaveOccurred())
		Expect(result.Created).To(Equal(0))
		Expect(queries).To(BeEmpty(), "a bare DROP is not executable DDL and never reaches the server")
	})

	It("applies idempotent ALTER MODIFY SETTING statements after the CREATEs (the settings-converge path)", func() {
		// The three-way split finding: CREATE IF NOT EXISTS can never
		// update SETTINGS on an existing table, so a bundle WITHOUT
		// ALTERs leaves live metadata diverged forever (each churn
		// re-materializes whatever the node's own CREATE happened to
		// carry). The bundle ships ALTER ... MODIFY SETTING after the
		// CREATEs and every pass re-asserts them until live matches.
		Expect(os.WriteFile(filepath.Join(dir, "bootstrap-ddl.sql"), []byte(
			`CREATE TABLE IF NOT EXISTS events (id String) ENGINE = ReplicatedMergeTree('/clickhouse/tables/01/events', '{replica}');
ALTER TABLE default.events MODIFY SETTING max_replicated_merges_in_queue = 3000, max_merge_selecting_sleep_ms = 10000;
`), 0644)).To(Succeed())

		result, err := ApplyBootstrapDDL(context.Background(), "writer-0", "writer")
		Expect(err).NotTo(HaveOccurred())
		Expect(result.Created).To(Equal(2))
		Expect(result.Failed).To(Equal(0))
		Expect(queries).To(ContainElement(
			"ALTER TABLE default.events MODIFY SETTING max_replicated_merges_in_queue = 3000, max_merge_selecting_sleep_ms = 10000"))
	})

	It("counts a failed ALTER so the next pass retries the converge", func() {
		Expect(os.WriteFile(filepath.Join(dir, "bootstrap-ddl.sql"), []byte(
			`ALTER TABLE default.events MODIFY SETTING max_replicated_merges_in_queue = 3000;
`), 0644)).To(Succeed())
		failOn["ALTER TABLE default.events MODIFY SETTING max_replicated_merges_in_queue = 3000"] =
			"Code: 60. Unknown table"
		// The exec fake keys on the TRIMMED statement (ApplyBootstrapDDL
		// trims each split statement before dispatch), so seed the
		// exact trimmed form.

		result, err := ApplyBootstrapDDL(context.Background(), "writer-0", "writer")
		Expect(err).NotTo(HaveOccurred())
		Expect(result.Failed).To(Equal(1))
		// The failed ALTER must NOT trigger the Code 253 drop-retry —
		// that path is CREATE-only (an ALTER has no replica conflict
		// shape, and dropping the node's znode over a settings miss
		// would be destructive).
		Expect(queries).NotTo(ContainElement(ContainSubstring("SYSTEM DROP REPLICA")))
	})

	It("refuses to emit SYSTEM DROP REPLICA '' when replica name is empty", func() {
		result := &DDLApplyResult{}
		ok := applyConflictRetry(context.Background(), "pw",
			"CREATE TABLE IF NOT EXISTS events (id String) ENGINE = ReplicatedMergeTree('/clickhouse/tables/01/events', '{replica}')",
			"", result)
		Expect(ok).To(BeFalse())
		Expect(queries).NotTo(ContainElement(ContainSubstring("SYSTEM DROP REPLICA ''")))
	})

	It("skips role-scoped ALTERs on excluded roles", func() {
		Expect(os.WriteFile(filepath.Join(dir, "bootstrap-ddl.sql"), []byte(
			`-- chinit:roles=writer,reader
ALTER TABLE default.staging_synth MODIFY SETTING s3queue_polling_max = 4;
`), 0644)).To(Succeed())

		result, err := ApplyBootstrapDDL(context.Background(), "writer-0", "backup")
		Expect(err).NotTo(HaveOccurred())
		Expect(result.Skipped).To(Equal(1))
		Expect(queries).To(BeEmpty())
	})
})

// errString is the minimal error the fake exec returns — the pass only
// needs err != nil and the query text to classify.
type errString struct{ s string }

func (e *errString) Error() string { return e.s }

var _ = Describe("ApplyBootstrapDDL role filter", func() {
	var (
		dir     string
		queries []string
		failOn  map[string]string
	)

	BeforeEach(func() {
		dir = filepath.Join(os.TempDir(), "chinit-apply-ddl-roles")
		Expect(os.MkdirAll(dir, 0755)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(dir, "bootstrap-ddl.sql"), []byte(
			`-- Fleet schema: role-universal (every server node)
CREATE TABLE IF NOT EXISTS events (id String) ENGINE = ReplicatedMergeTree('/clickhouse/tables/{shard}/events', '{replica}');
-- The staged-ingest pipeline: writers/readers ONLY (the backup's IAM
-- cannot list staging — Code 499 AccessDenied on the queue's poll).
--    chinit:roles=writer,reader
CREATE TABLE IF NOT EXISTS staging_synth (id String) ENGINE = S3Queue('s3://x/staging/', 'NDJSON', 'auto');
CREATE MATERIALIZED VIEW IF NOT EXISTS staging_to_events TO default.events AS SELECT id FROM staging_synth;
`), 0644)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(dir, "admin-password"), []byte("sekrit\n"), 0600)).To(Succeed())

		queries = nil
		failOn = map[string]string{}
		execClientFn = func(_ context.Context, _, query string) (string, error) {
			queries = append(queries, query)
			if msg, ok := failOn[query]; ok {
				return msg, &errString{msg}
			}
			return "", nil
		}
		DeferCleanup(func() { execClientFn = nil })

		ddl := bootstrapDDLPath
		pw := bootstrapAdminPasswordPath
		DeferCleanup(func() { bootstrapDDLPath, bootstrapAdminPasswordPath = ddl, pw })
		bootstrapDDLPath = func() string { return filepath.Join(dir, "bootstrap-ddl.sql") }
		bootstrapAdminPasswordPath = func() string { return filepath.Join(dir, "admin-password") }
	})

	It("skips the staging pipeline on the backup role", func() {
		result, err := ApplyBootstrapDDL(context.Background(), "backup-0", "backup")
		Expect(err).NotTo(HaveOccurred())
		Expect(result.Created).To(Equal(1), "only the role-universal events table")
		Expect(result.Skipped).To(Equal(2), "the queue + MV are writer/reader-tagged")
		for _, q := range queries {
			Expect(q).NotTo(ContainSubstring("staging"), "no staging object on the backup")
		}
	})

	It("applies the staging pipeline on the writer role", func() {
		result, err := ApplyBootstrapDDL(context.Background(), "writer-0", "writer")
		Expect(err).NotTo(HaveOccurred())
		Expect(result.Created).To(Equal(3))
		Expect(result.Skipped).To(Equal(0))
	})

	It("applies the staging pipeline on the reader role", func() {
		result, err := ApplyBootstrapDDL(context.Background(), "reader-0", "reader")
		Expect(err).NotTo(HaveOccurred())
		Expect(result.Created).To(Equal(3))
		Expect(result.Skipped).To(Equal(0))
	})
})
