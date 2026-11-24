// schema.go —— 建表 DDL（参考表，幂等）。
//
// 逐表对应关系见 doc.go。与 pgstore 的 pgref_* 同形：同样的键、同样的条件写、
// 同样的「一条带条件的 upsert 承担比较与写入」形态——只有方言不同。

package sqliteadapter

import (
	"context"
	"database/sql"
	"fmt"
)

// schemaDDL 一套参考表（幂等：全部 CREATE TABLE IF NOT EXISTS）。
//
// 刻意放在 Go 侧而不是 init.sql：契约套件与参考部署都从这条路径建表，
// 单一来源，宿主抄它时不会抄到一份没人维护的 SQL 文件。
const schemaDDL = `
-- 工具调用账本（invocation.Store 的落点，P0-09/P0-10 闭合）。
-- 与 pgref_invocation 同形：同样的键（session_id + call_id）、同样的列语义、
-- 同样的「一条带条件的 UPDATE 承担比较与写入」形态——只有方言不同。
-- seq 同样由**存储侧铸造**（与 PG 侧的 IDENTITY 列同款职责，落到方言：
-- SQLite 用自增列做全局单调计数，PG 用 IDENTITY）。
CREATE TABLE IF NOT EXISTS gak_invocation (
    seq         INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id  TEXT    NOT NULL,
    call_id     TEXT    NOT NULL,
    tool        TEXT    NOT NULL DEFAULT '',
    args_digest TEXT    NOT NULL DEFAULT '',
    args        BLOB,
    state       TEXT    NOT NULL,
    result      BLOB,
    reason      TEXT    NOT NULL DEFAULT '',
    UNIQUE (session_id, call_id)
);

CREATE TABLE IF NOT EXISTS gak_board_task (
    id     TEXT PRIMARY KEY,
    title  TEXT    NOT NULL DEFAULT '',
    deps   TEXT    NOT NULL DEFAULT '[]',
    state  TEXT    NOT NULL,
    owner  TEXT,
    token  TEXT,
    fence  INTEGER NOT NULL DEFAULT 0,
    until  INTEGER,
    seq    INTEGER NOT NULL DEFAULT 0,
    run_id TEXT    NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS gak_effect_action (
    tenant          TEXT    NOT NULL DEFAULT '',
    key             TEXT    NOT NULL,
    done            INTEGER NOT NULL DEFAULT 0,
    result          BLOB,
    uncertain       INTEGER NOT NULL DEFAULT 0,
    reason          TEXT    NOT NULL DEFAULT '',
    intent_resource TEXT,
    intent_fence    INTEGER,
    intent_holder   TEXT,
    intent_expires  INTEGER,
    recon_holder    TEXT,
    recon_expires   INTEGER,
    id_resource     TEXT,
    id_safety       INTEGER,
    id_payload_hash TEXT,
    PRIMARY KEY (tenant, key)
);

CREATE TABLE IF NOT EXISTS gak_effect_resource (
    tenant   TEXT    NOT NULL DEFAULT '',
    resource TEXT    NOT NULL,
    fence    INTEGER NOT NULL DEFAULT 0,
    holder   TEXT,
    until    INTEGER,
    PRIMARY KEY (tenant, resource)
);

CREATE TABLE IF NOT EXISTS gak_downstream (
    tenant     TEXT    NOT NULL DEFAULT '',
    resource   TEXT    NOT NULL,
    seen_fence INTEGER NOT NULL,
    value      TEXT    NOT NULL DEFAULT '',
    PRIMARY KEY (tenant, resource)
);

CREATE TABLE IF NOT EXISTS gak_snapshot (
    run_id TEXT PRIMARY KEY,
    seq    INTEGER NOT NULL,
    status TEXT    NOT NULL DEFAULT '',
    done   TEXT    NOT NULL DEFAULT '[]',
    fields TEXT    NOT NULL DEFAULT ''
);

-- 每租户一行水位：事件 ID 的铸造点（UPDATE … RETURNING 拿号并持行锁到提交）。
-- 回滚时 committed 随事务回滚 ⇒ 下次分配复用同一 ID ⇒ 序列空洞结构性不存在。
CREATE TABLE IF NOT EXISTS gak_event_watermark (
    tenant    TEXT PRIMARY KEY,
    committed INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS gak_event (
    tenant      TEXT    NOT NULL DEFAULT '',
    id          INTEGER NOT NULL,
    session_id  TEXT    NOT NULL DEFAULT '',
    case_id     TEXT    NOT NULL DEFAULT '',
    step_id     INTEGER,
    type        TEXT    NOT NULL DEFAULT '',
    level       TEXT    NOT NULL DEFAULT 'info',
    payload     TEXT    NOT NULL DEFAULT '{}',
    occurred_at INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (tenant, id)
);

CREATE TABLE IF NOT EXISTS gak_tail_cursor (
    consumer   TEXT PRIMARY KEY,
    cursor_pos INTEGER NOT NULL DEFAULT 0 CHECK (cursor_pos >= 0)
);

CREATE TABLE IF NOT EXISTS gak_run (
    id     TEXT PRIMARY KEY,
    status TEXT    NOT NULL DEFAULT '',
    gen    INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS gak_control_version (
    id  INTEGER PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    ver INTEGER NOT NULL DEFAULT 0
);

INSERT INTO gak_control_version (id) VALUES (1) ON CONFLICT DO NOTHING;

CREATE TABLE IF NOT EXISTS gak_control_effect (
    key       TEXT PRIMARY KEY,
    uncertain INTEGER NOT NULL DEFAULT 0,
    unsafe    INTEGER NOT NULL DEFAULT 0,
    resolved  TEXT    NOT NULL DEFAULT ''
);
`

// EnsureSchema 建好全部参考表（幂等，可重复执行）。
//
// 全部语句在**一个事务**里执行：建表中途失败不留半截 schema。
// DDL 的写锁升级在 SQLITE_BUSY 上不等 busy_timeout（实测），故建表只在
// 装配期做，不在运行期做。
func (s *Store) EnsureSchema(ctx context.Context) error {
	if err := s.ensureSchema(ctx, schemaDDL); err != nil {
		return fmt.Errorf("sqliteadapter: 建表失败: %w", err)
	}
	return nil
}

// ensureSchema 在一个事务里执行给定的 DDL（可注入，便于测试中途失败形态）。
//
// DDL 做成参数是判据需求，不是设计洁癖：只有能注入「先建一张表、再执行一条
// 语法错误语句」的多语句 DDL，用例才能区分「一个事务内建表」与「裸跑逐条
// 提交」——后者在 autocommit 下会留下半截 schema（正是上面那句注释要防的事）。
func (s *Store) ensureSchema(ctx context.Context, ddl string) error {
	return s.exec().run(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, ddl)
		return err
	})
}
