package sqlstore

// Schema definitions stay together so storage changes can be reviewed with the
// migration order. These initialize the current schema on an empty database;
// append future upgrades in migrations.go. pwnmesh_* names are active SQL identities.

const schema = `
CREATE TABLE IF NOT EXISTS settings(intent_timeout INTEGER NOT NULL DEFAULT 15,reason_timeout INTEGER NOT NULL DEFAULT 15);
` + settingsSeed + `
CREATE TABLE IF NOT EXISTS projects(id TEXT PRIMARY KEY,title TEXT NOT NULL,status TEXT NOT NULL DEFAULT 'active',bootstrap_enabled INTEGER NOT NULL DEFAULT 1,created_at TEXT NOT NULL,reason_worker TEXT,reason_trigger TEXT,reason_started_at TEXT,reason_last_heartbeat_at TEXT);
CREATE TABLE IF NOT EXISTS facts(id TEXT NOT NULL,project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,description TEXT NOT NULL,PRIMARY KEY(id,project_id));
CREATE TABLE IF NOT EXISTS intents(id TEXT NOT NULL,project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,to_fact_id TEXT,description TEXT NOT NULL,creator TEXT NOT NULL,worker TEXT,last_heartbeat_at TEXT,created_at TEXT NOT NULL,concluded_at TEXT,PRIMARY KEY(id,project_id));
CREATE TABLE IF NOT EXISTS intent_sources(intent_id TEXT NOT NULL,project_id TEXT NOT NULL,fact_id TEXT NOT NULL,PRIMARY KEY(intent_id,project_id,fact_id),FOREIGN KEY(intent_id,project_id) REFERENCES intents(id,project_id) ON DELETE CASCADE);
CREATE INDEX IF NOT EXISTS intent_sources_project ON intent_sources(project_id);
CREATE TABLE IF NOT EXISTS hints(id TEXT NOT NULL,project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,content TEXT NOT NULL,creator TEXT NOT NULL,created_at TEXT NOT NULL,PRIMARY KEY(id,project_id));
CREATE INDEX IF NOT EXISTS facts_project ON facts(project_id);
CREATE INDEX IF NOT EXISTS intents_project ON intents(project_id,created_at);
CREATE INDEX IF NOT EXISTS hints_project ON hints(project_id,created_at);
CREATE INDEX IF NOT EXISTS intents_active_leases ON intents(project_id) WHERE to_fact_id IS NULL AND worker IS NOT NULL;
CREATE TABLE IF NOT EXISTS counters(name TEXT PRIMARY KEY,value INTEGER NOT NULL DEFAULT 0);
INSERT OR IGNORE INTO counters(name,value) VALUES('project',0);
CREATE TABLE IF NOT EXISTS scoped_counters(project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,kind TEXT NOT NULL,value INTEGER NOT NULL DEFAULT 0,PRIMARY KEY(project_id,kind));
CREATE TABLE IF NOT EXISTS pwnmesh_revoked_runs(project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,worker TEXT NOT NULL,PRIMARY KEY(project_id,worker));`

const settingsSeed = `INSERT OR IGNORE INTO settings(rowid,intent_timeout,reason_timeout) VALUES(1,15,15);`

const stateSchema = `
CREATE TABLE IF NOT EXISTS pwnmesh_state(project_id TEXT PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,data TEXT NOT NULL,revision INTEGER NOT NULL DEFAULT 0,decision_revision INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS pwnmesh_state_actions(project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,idempotency_key TEXT NOT NULL,request TEXT NOT NULL,response TEXT NOT NULL,PRIMARY KEY(project_id,idempotency_key));
CREATE TABLE IF NOT EXISTS pwnmesh_state_events(project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,revision INTEGER NOT NULL,event TEXT NOT NULL,PRIMARY KEY(project_id,revision));`

const executionSchema = `CREATE TABLE IF NOT EXISTS pwnmesh_executions(
 project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
 id TEXT NOT NULL,namespace TEXT NOT NULL,backend TEXT NOT NULL,kind TEXT NOT NULL,intent TEXT NOT NULL,
 lease TEXT NOT NULL,job BLOB NOT NULL,retry_key TEXT NOT NULL,status TEXT NOT NULL,result BLOB,
 resumes INTEGER NOT NULL DEFAULT 0,created_at TEXT NOT NULL,updated_at TEXT NOT NULL,
 generation INTEGER NOT NULL DEFAULT 0,input_revision INTEGER NOT NULL DEFAULT 0,
 decision_revision INTEGER NOT NULL DEFAULT 0,state_version TEXT NOT NULL DEFAULT '',
 has_state INTEGER NOT NULL DEFAULT 0,fact_count INTEGER NOT NULL DEFAULT 0,
 hint_count INTEGER NOT NULL DEFAULT 0,open_count INTEGER NOT NULL DEFAULT 0,
 metadata_version INTEGER NOT NULL DEFAULT 0,
 PRIMARY KEY(project_id,id));
CREATE TABLE IF NOT EXISTS pwnmesh_paused_executions(
 project_id TEXT NOT NULL,execution_id TEXT NOT NULL,
 PRIMARY KEY(project_id,execution_id),
 FOREIGN KEY(project_id,execution_id) REFERENCES pwnmesh_executions(project_id,id) ON DELETE CASCADE);` + executionIndexSchema + inputSnapshotSchema + decisionReadSchema

const executionIndexSchema = `CREATE INDEX IF NOT EXISTS pwnmesh_execution_pending ON pwnmesh_executions(namespace) WHERE ` + PendingExecutionSQL + `;
CREATE INDEX IF NOT EXISTS pwnmesh_execution_task ON pwnmesh_executions(namespace,project_id,kind,intent,status,created_at);
CREATE INDEX IF NOT EXISTS pwnmesh_execution_retry ON pwnmesh_executions(namespace,project_id,retry_key,generation);
CREATE INDEX IF NOT EXISTS pwnmesh_execution_decision ON pwnmesh_executions(namespace,project_id,generation,kind,status,created_at);
CREATE INDEX IF NOT EXISTS pwnmesh_execution_version ON pwnmesh_executions(namespace,project_id,kind,state_version);
CREATE INDEX IF NOT EXISTS pwnmesh_execution_step ON pwnmesh_executions(project_id,intent) WHERE kind!='reason';`

const inputSnapshotSchema = `CREATE TABLE IF NOT EXISTS pwnmesh_input_snapshots(
 project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
 id TEXT NOT NULL,metadata BLOB NOT NULL,state BLOB NOT NULL,
 PRIMARY KEY(project_id,id));
CREATE TABLE IF NOT EXISTS pwnmesh_snapshot_chunks(
 project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
 sha256 TEXT NOT NULL,data BLOB NOT NULL,
 PRIMARY KEY(project_id,sha256));`

const decisionReadSchema = `CREATE TABLE IF NOT EXISTS pwnmesh_decision_read_views(
 project_id TEXT NOT NULL,execution_id TEXT NOT NULL,state_version TEXT NOT NULL,state BLOB NOT NULL,
 PRIMARY KEY(project_id,execution_id),
 FOREIGN KEY(project_id,execution_id) REFERENCES pwnmesh_executions(project_id,id) ON DELETE CASCADE);` + pruneDecisionReadViews + decisionReadTriggerSchema

const decisionReadTriggerSchema = `CREATE TRIGGER IF NOT EXISTS pwnmesh_decision_read_terminal AFTER UPDATE OF status ON pwnmesh_executions
 WHEN NEW.status NOT IN ('prepared','running','retryable','result_pending') BEGIN
 DELETE FROM pwnmesh_decision_read_views WHERE project_id=NEW.project_id AND execution_id=NEW.id;
END;
CREATE TRIGGER IF NOT EXISTS pwnmesh_decision_read_revoked AFTER INSERT ON pwnmesh_revoked_runs BEGIN
 DELETE FROM pwnmesh_decision_read_views WHERE project_id=NEW.project_id AND execution_id IN
  (SELECT id FROM pwnmesh_executions WHERE project_id=NEW.project_id AND lease=NEW.worker);
END;`

const pruneDecisionReadViews = `DELETE FROM pwnmesh_decision_read_views
 WHERE EXISTS(SELECT 1 FROM pwnmesh_executions e
  WHERE e.project_id=pwnmesh_decision_read_views.project_id AND e.id=pwnmesh_decision_read_views.execution_id
  AND (e.status NOT IN ('prepared','running','retryable','result_pending')
   OR EXISTS(SELECT 1 FROM pwnmesh_revoked_runs r WHERE r.project_id=e.project_id AND r.worker=e.lease)));`

// Scenario metadata is optional; missing rows do not invent a project scenario.
const projectMetadataSchema = `CREATE TABLE IF NOT EXISTS pwnmesh_project_metadata(
 project_id TEXT PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
 scenario TEXT NOT NULL CHECK(scenario IN ('ctf','pentest','audit')));` + executionChangeSchema

// Keep only the latest change per execution. A persistent sequence observes
// updates within one second, including result observations without updated_at.
const executionChangeSchema = `
CREATE TABLE IF NOT EXISTS pwnmesh_ui_execution_revisions(
 project_id TEXT PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
 revision INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS pwnmesh_ui_execution_changes(
 project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
 execution_id TEXT NOT NULL,revision INTEGER NOT NULL,deleted INTEGER NOT NULL DEFAULT 0,
 PRIMARY KEY(project_id,execution_id));
CREATE INDEX IF NOT EXISTS pwnmesh_ui_execution_changes_revision ON pwnmesh_ui_execution_changes(project_id,revision);
` + executionChangeBackfill + executionChangeTriggerSchema

const executionChangeTriggerSchema = `CREATE TRIGGER IF NOT EXISTS pwnmesh_ui_execution_insert AFTER INSERT ON pwnmesh_executions BEGIN
 INSERT INTO pwnmesh_ui_execution_revisions VALUES(NEW.project_id,1)
 ON CONFLICT(project_id) DO UPDATE SET revision=revision+1;
 INSERT INTO pwnmesh_ui_execution_changes VALUES(NEW.project_id,NEW.id,(SELECT revision FROM pwnmesh_ui_execution_revisions WHERE project_id=NEW.project_id),0)
 ON CONFLICT(project_id,execution_id) DO UPDATE SET revision=excluded.revision,deleted=0;
END;
CREATE TRIGGER IF NOT EXISTS pwnmesh_ui_execution_update AFTER UPDATE ON pwnmesh_executions BEGIN
 INSERT INTO pwnmesh_ui_execution_revisions VALUES(NEW.project_id,1)
 ON CONFLICT(project_id) DO UPDATE SET revision=revision+1;
 INSERT INTO pwnmesh_ui_execution_changes VALUES(NEW.project_id,NEW.id,(SELECT revision FROM pwnmesh_ui_execution_revisions WHERE project_id=NEW.project_id),0)
 ON CONFLICT(project_id,execution_id) DO UPDATE SET revision=excluded.revision,deleted=0;
END;
CREATE TRIGGER IF NOT EXISTS pwnmesh_ui_execution_delete AFTER DELETE ON pwnmesh_executions
WHEN EXISTS(SELECT 1 FROM projects WHERE id=OLD.project_id) BEGIN
 INSERT INTO pwnmesh_ui_execution_revisions VALUES(OLD.project_id,1)
 ON CONFLICT(project_id) DO UPDATE SET revision=revision+1;
 INSERT INTO pwnmesh_ui_execution_changes VALUES(OLD.project_id,OLD.id,(SELECT revision FROM pwnmesh_ui_execution_revisions WHERE project_id=OLD.project_id),1)
 ON CONFLICT(project_id,execution_id) DO UPDATE SET revision=excluded.revision,deleted=1;
END;`

const executionChangeBackfill = `INSERT OR IGNORE INTO pwnmesh_ui_execution_revisions(project_id,revision)
 SELECT project_id,MAX(rowid) FROM pwnmesh_executions GROUP BY project_id;
INSERT OR IGNORE INTO pwnmesh_ui_execution_changes(project_id,execution_id,revision)
 SELECT project_id,id,rowid FROM pwnmesh_executions;`

const restartSchema = `CREATE TABLE IF NOT EXISTS pwnmesh_project_rounds(
 project_id TEXT PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
 generation INTEGER NOT NULL DEFAULT 0,restarted_at TEXT NOT NULL);` + roundHistorySchema

const roundHistorySchema = `
CREATE TABLE IF NOT EXISTS pwnmesh_round_history(
 project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
 generation INTEGER NOT NULL,archived_at TEXT NOT NULL,
 PRIMARY KEY(project_id,generation));
CREATE TABLE IF NOT EXISTS pwnmesh_round_entries(
 project_id TEXT NOT NULL,generation INTEGER NOT NULL,kind TEXT NOT NULL,id TEXT NOT NULL,
 data BLOB NOT NULL,sha256 TEXT NOT NULL,
 PRIMARY KEY(project_id,generation,kind,id),
 FOREIGN KEY(project_id,generation) REFERENCES pwnmesh_round_history(project_id,generation) ON DELETE CASCADE);`

const terminationSchema = `CREATE TABLE IF NOT EXISTS pwnmesh_project_termination(
 project_id TEXT PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
 terminated_at TEXT NOT NULL);`

const orchestrationSchema = `CREATE TABLE IF NOT EXISTS pwnmesh_project_orchestration(
 project_id TEXT PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
 version INTEGER NOT NULL,curator_worker TEXT,curator_trigger TEXT,curator_started_at TEXT,curator_heartbeat TEXT);`

const inputFileSchema = `
CREATE TABLE IF NOT EXISTS pwnmesh_input_files(
 project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
 id TEXT NOT NULL,name TEXT NOT NULL,size INTEGER NOT NULL,sha256 TEXT NOT NULL,
 created_at TEXT NOT NULL,data BLOB NOT NULL,
 PRIMARY KEY(project_id,id),UNIQUE(project_id,name,sha256));
CREATE TABLE IF NOT EXISTS pwnmesh_input_batches(
 project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
 idempotency_key TEXT NOT NULL,request_sha256 TEXT NOT NULL,response TEXT NOT NULL,
 PRIMARY KEY(project_id,idempotency_key));`

const assetSchema = `
CREATE TABLE IF NOT EXISTS pwnmesh_assets(
 project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
 id TEXT NOT NULL,kind TEXT NOT NULL,value TEXT NOT NULL,method TEXT NOT NULL,
 PRIMARY KEY(project_id,id),UNIQUE(project_id,kind,value,method));
CREATE TABLE IF NOT EXISTS pwnmesh_asset_anchors(
 project_id TEXT NOT NULL,generation INTEGER NOT NULL,node_kind TEXT NOT NULL,node_id TEXT NOT NULL,asset_id TEXT NOT NULL,
 PRIMARY KEY(project_id,generation,node_kind,node_id,asset_id),
 FOREIGN KEY(project_id,asset_id) REFERENCES pwnmesh_assets(project_id,id) ON DELETE CASCADE);
CREATE INDEX IF NOT EXISTS pwnmesh_asset_anchors_asset ON pwnmesh_asset_anchors(project_id,generation,asset_id);`

const deploymentSchema = `CREATE TABLE IF NOT EXISTS pwnmesh_installation(
 singleton INTEGER PRIMARY KEY CHECK(singleton=1),identity TEXT NOT NULL);
` + installationSeed + `
CREATE TABLE IF NOT EXISTS pwnmesh_dispatchers(namespace TEXT PRIMARY KEY,holder TEXT NOT NULL,expires_at TEXT NOT NULL);`

const installationSeed = `INSERT OR IGNORE INTO pwnmesh_installation(singleton,identity) VALUES(1,lower(hex(randomblob(16))));`
