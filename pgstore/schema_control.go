// schema_control.go —— 控制面三张表（版本锚 / 效果裁决账 / 介入留痕账）。
//
// 从 schema.go 拆出：schemaDDL 超 400 行结构阈值。控制面表随权威接线
// （#268 / ForceReleaseAtomic）独立演进，按关注点分文件正是它们的归处。
package pgstore

// controlDDL 控制面表组：与 schemaDDL 同一事务执行（EnsureSchema 内），
// 幂等（CREATE TABLE IF NOT EXISTS / ON CONFLICT DO NOTHING）。
const controlDDL = `
-- 版本锚（全局修订号）：三个 Commit* 在同一个事务里「校验版本 → 留痕 →
-- 写状态 → 推进版本」。快照之后目标被任何副本改过 ⇒ 提交必失败。
CREATE TABLE IF NOT EXISTS pgref_control_version (
    id  SMALLINT PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    ver BIGINT   NOT NULL DEFAULT 0
);

INSERT INTO pgref_control_version (id) VALUES (1) ON CONFLICT DO NOTHING;

-- 控制面效果裁决账（SnapshotEffect / CommitEffect）。权威接线（#268）应
-- 优先走 NewAuthoritativeStorePlane + SideEffectAuthority：裁决落到副作用
-- 闸读取的同一份状态，本表只是 Store 契约的独立后端落点。
CREATE TABLE IF NOT EXISTS pgref_control_effect (
    key       TEXT PRIMARY KEY,
    uncertain BOOLEAN NOT NULL DEFAULT FALSE,
    unsafe    BOOLEAN NOT NULL DEFAULT FALSE,
    resolved  TEXT    NOT NULL DEFAULT ''
);

-- 权威介入留痕账（ForceReleaseAtomic，P0-A）：与任务释放**同一事务**写入——
-- 审计行落库失败 ⇒ 释放一并回滚（「介入返回 error ⇒ 可观测状态未变」）。
-- 外部审计平台经 Outbox 异步消费本表，不承担事务一致性职责。
CREATE TABLE IF NOT EXISTS pgref_control_audit (
    id       BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    at       TIMESTAMPTZ NOT NULL,
    operator TEXT      NOT NULL CHECK (operator <> ''),
    action   TEXT      NOT NULL,
    target   TEXT      NOT NULL,
    basis    TEXT      NOT NULL,
    detail   TEXT      NOT NULL DEFAULT ''
);
`
