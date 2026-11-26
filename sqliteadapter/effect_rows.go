// effect_rows.go —— effect 存储的行级读写与凭据联查。
//
// 判据归属：行投影、半写行守卫、三项联查、落库写回都在本文件；事务骨架与
// 判定顺序在 effect.go（Acquire）与 effect_lease.go / effect_reconcile.go。
//
// # 方言映射（PG → SQLite）
//
//   - $1/$2 → ?；TIMESTAMPTZ → INTEGER（Unix 毫秒）；BYTEA → BLOB；
//     SELECT … FOR UPDATE → **不加**（BEGIN IMMEDIATE 已持写锁）。
//   - 可空列用 sql.Null* 承载：「NULL 与零值同形」在 SQLite 里尤其危险
//     （零值到期 = 不限期，当成已过期会让两个执行者同时认领），故一律用
//     Null* 显式区分。
package sqliteadapter

import (
	"context"
	"database/sql"
	"time"

	"github.com/oekit/pkg/errors"

	"github.com/yi-nology/goagent-kernel/sideeffect"
)

// ensureActionRow 保证动作行存在（Acquire 的自增主键语义：「没做过」就是空行——
// 首个动作顺手把行建出来）。
//
// INSERT 同时登记动作身份（首次见到这个 Key 的调用就是身份登记时刻，即便这次
// Acquire 的结果是 contended——身份不等授予）。冲突（已存在）不更新：身份以
// 先到者的登记为准，后到的不一致者由 lockAction 内的 ensureIdentity 拒掉
// （ErrActionConflict）。
func ensureActionRow(ctx context.Context, tx *sql.Tx, act sideeffect.Action) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO gak_effect_action (tenant, key, id_resource, id_safety, id_payload_hash)
		 VALUES (?, ?, ?, ?, ?) ON CONFLICT (tenant, key) DO NOTHING`,
		act.Tenant, string(act.Key), act.Resource, int64(act.RetrySafety), sideeffect.HashPayload(act.Payload))
	if err != nil {
		return errors.Wrap(err, "sqliteadapter: 登记动作行失败", "key", string(act.Key))
	}
	return nil
}

// ensureIdentity 动作身份校验（调用方已读到动作行）：未登记 → 补登记（老数据
// 行的兼容路径）；已登记 → 与本次 act 比对，不一致即 ErrActionConflict。
//
// 比对在 lockAction 内完成 ⇒ 每个入口拿到行的那一刻就先问身份，「已完成 →
// 重放」等任何判定都在它之后——把旧身份的结果交给新身份的调用方是答非所问
// （Store.Acquire 契约「身份不可变」）。
func ensureIdentity(ctx context.Context, tx *sql.Tx, a *actionRow, act sideeffect.Action) error {
	if !a.idResource.Valid || !a.idSafety.Valid || !a.idPayloadHash.Valid {
		res := act.Resource
		safety := int64(act.RetrySafety)
		hash := sideeffect.HashPayload(act.Payload)
		if _, err := tx.ExecContext(ctx,
			`UPDATE gak_effect_action SET id_resource = ?, id_safety = ?, id_payload_hash = ?
			  WHERE tenant = ? AND key = ? AND id_resource IS NULL`,
			res, safety, hash, act.Tenant, string(act.Key)); err != nil {
			return errors.Wrap(err, "sqliteadapter: 登记动作身份失败", "key", string(act.Key))
		}
		a.idResource = sql.NullString{String: res, Valid: true}
		a.idSafety = sql.NullInt64{Int64: safety, Valid: true}
		a.idPayloadHash = sql.NullString{String: hash, Valid: true}
		return nil
	}
	if a.idResource.String != act.Resource || int(a.idSafety.Int64) != int(act.RetrySafety) ||
		a.idPayloadHash.String != sideeffect.HashPayload(act.Payload) {
		return errors.Wrap(sideeffect.ErrActionConflict,
			"同 Key 的动作身份与首次登记不一致（这不是重试，是另一个动作）",
			"key", string(act.Key),
			"登记 resource", a.idResource.String, "本次 resource", act.Resource,
			"登记 safety", sideeffect.RetrySafety(a.idSafety.Int64), "本次 safety", act.RetrySafety)
	}
	return nil
}

// ensureResourceRow 保证资源行存在（Acquirer 的自增主键语义：「没人占」就是空行）。
func ensureResourceRow(ctx context.Context, tx *sql.Tx, tenantID, resource string) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO gak_effect_resource (tenant, resource) VALUES (?, ?)
		 ON CONFLICT (tenant, resource) DO NOTHING`,
		tenantID, resource)
	if err != nil {
		return errors.Wrap(err, "sqliteadapter: 登记资源行失败", "resource", resource)
	}
	return nil
}

// actionRow Key 级状态的一行（列语义见 schema.go 的表定义）。
type actionRow struct {
	done      bool
	result    []byte
	hasResult bool
	uncertain bool
	reason    string

	// 动作身份（首次登记，永久事实；未登记 = 老数据行，由 ensureIdentity
	// 首见时补登记）。三者与 ErrActionConflict 对应。
	idResource    sql.NullString
	idSafety      sql.NullInt64
	idPayloadHash sql.NullString

	intentResource sql.NullString
	intentFence    sql.NullInt64
	intentHolder   sql.NullString
	intentExpires  sql.NullInt64

	reconHolder  sql.NullString
	reconExpires sql.NullInt64
}

// intentColumnsFilled 执行意图四列中非 NULL 的列数。
func (a *actionRow) intentColumnsFilled() int {
	n := 0
	if a.intentResource.Valid {
		n++
	}
	if a.intentFence.Valid {
		n++
	}
	if a.intentHolder.Valid {
		n++
	}
	if a.intentExpires.Valid {
		n++
	}
	return n
}

// checkIntentColumns 半写行 fail-closed：既不解引用 NULL，也不当空行放行。
// 四列全空是正常形态（没有未裁决的尝试）；全有才是可判定的意图。
func (a *actionRow) checkIntentColumns() error {
	if n := a.intentColumnsFilled(); n != 0 && n != 4 {
		return errors.Wrap(ErrIntentIncomplete, "执行意图四列未同存同缺（半写行）——拒绝判定", "filled", n)
	}
	return nil
}

// hasIntent 该 Key 上是否挂着一次未裁决的尝试（四列同时判）。
func (a *actionRow) hasIntent() bool { return a.intentColumnsFilled() == 4 }

// hasRecon 该 Key 上是否挂着对账占用。
func (a *actionRow) hasRecon() bool { return a.reconExpires.Valid }

// intentExpiresAt 意图的到期时刻（存储时钟口径；Unix 毫秒 → time.Time）。
func (a *actionRow) intentExpiresAt() time.Time {
	return time.UnixMilli(a.intentExpires.Int64).UTC()
}

// reconExpiresAt 对账占用的到期时刻（存储时钟口径）。
func (a *actionRow) reconExpiresAt() time.Time {
	return time.UnixMilli(a.reconExpires.Int64).UTC()
}

// resultValue 把已存的结果投影成契约值（拷贝语义：调用方改它不影响库）。
func (a *actionRow) resultValue() sideeffect.Result {
	var res sideeffect.Result
	if a.hasResult {
		res.Payload = append([]byte(nil), a.result...)
	}
	return res
}

// clearIntent 撤销执行意图四列（同存同空——半写行是存储侧损坏，不是本包能
// 产生的状态）。
func (a *actionRow) clearIntent() {
	a.intentResource, a.intentFence, a.intentHolder, a.intentExpires =
		sql.NullString{}, sql.NullInt64{}, sql.NullString{}, sql.NullInt64{}
}

// clearRecon 撤销对账占用两列。
func (a *actionRow) clearRecon() {
	a.reconHolder, a.reconExpires = sql.NullString{}, sql.NullInt64{}
}

// lockAction 锁住并读取 Key 级状态行（调用前必须 ensureActionRow）。
//
// lockAction 读出动作行，**读后立刻校验动作身份**（见 ensureIdentity）——所有
// 入口共用这一处，身份判定天然先于任何状态判定。
func lockAction(ctx context.Context, tx *sql.Tx, act sideeffect.Action) (*actionRow, error) {
	row := tx.QueryRowContext(ctx,
		`SELECT done, result, uncertain, reason,
		        id_resource, id_safety, id_payload_hash,
		        intent_resource, intent_fence, intent_holder, intent_expires,
		        recon_holder, recon_expires
		   FROM gak_effect_action WHERE tenant = ? AND key = ?`,
		act.Tenant, string(act.Key))
	var (
		a                                                       actionRow
		done, uncertain                                         int64
		reason                                                  string
		idResource, idPayloadHash, intentResource, intentHolder sql.NullString
		idSafety, intentFence, intentExpires, reconExpires      sql.NullInt64
		reconHolder                                             sql.NullString
	)
	if err := row.Scan(&done, &a.result, &uncertain, &reason,
		&idResource, &idSafety, &idPayloadHash,
		&intentResource, &intentFence, &intentHolder, &intentExpires,
		&reconHolder, &reconExpires); err != nil {
		return nil, errors.Wrap(err, "sqliteadapter: 读动作行失败", "key", string(act.Key))
	}
	a.done = done != 0
	a.uncertain = uncertain != 0
	a.reason = reason
	a.hasResult = a.result != nil
	a.idResource, a.idSafety, a.idPayloadHash = idResource, idSafety, idPayloadHash
	a.intentResource, a.intentFence, a.intentHolder, a.intentExpires =
		intentResource, intentFence, intentHolder, intentExpires
	a.reconHolder, a.reconExpires = reconHolder, reconExpires
	// 半写意图行在这里就 fail-closed（所有入口共用 lockAction，漏不掉）：
	// 继续判下去要么解引用 NULL panics，要么把半写行当「无意图」放行。
	if err := a.checkIntentColumns(); err != nil {
		return nil, errors.Wrap(err, "sqliteadapter: 动作行不可判定", "key", string(act.Key))
	}
	if err := ensureIdentity(ctx, tx, &a, act); err != nil {
		return nil, err
	}
	return &a, nil
}

// resourceRow 资源级状态的一行。
type resourceRow struct {
	fence  int64
	holder sql.NullString
	until  sql.NullInt64
}

// held 该资源当前是否被占着且租约未过期（空 holder = 空闲）。
//
// ⚠ 到期时刻为零值 = 不限期（PG 的零值 until 同款语义）：当成「已过期」会让
// 两个执行者同时认领同一资源。
func (r *resourceRow) held(now time.Time) bool {
	return r.holder.Valid && r.until.Valid && time.UnixMilli(r.until.Int64).UTC().After(now)
}

// lockResource 锁住并读取资源级状态行（调用前必须 ensureResourceRow）。
func lockResource(ctx context.Context, tx *sql.Tx, tenantID, resource string) (*resourceRow, error) {
	row := tx.QueryRowContext(ctx,
		`SELECT fence, holder, until FROM gak_effect_resource WHERE tenant = ? AND resource = ?`,
		tenantID, resource)
	var (
		r      resourceRow
		holder sql.NullString
		until  sql.NullInt64
	)
	if err := row.Scan(&r.fence, &holder, &until); err != nil {
		return nil, errors.Wrap(err, "sqliteadapter: 读资源行失败", "resource", resource)
	}
	r.holder, r.until = holder, until
	return &r, nil
}

// writeResource 把资源级状态写回（调用方持写锁，无条件写即可）。
func writeResource(ctx context.Context, tx *sql.Tx, tenantID, resource string, rr *resourceRow) error {
	_, err := tx.ExecContext(ctx,
		`UPDATE gak_effect_resource SET fence = ?, holder = ?, until = ?
		  WHERE tenant = ? AND resource = ?`,
		rr.fence, nullString(rr.holder), nullInt(rr.until), tenantID, resource)
	if err != nil {
		return errors.Wrap(err, "sqliteadapter: 写资源行失败", "resource", resource)
	}
	return nil
}

// writeAction 把 Key 级状态写回（调用方持写锁，无条件写即可）。
func writeAction(ctx context.Context, tx *sql.Tx, tenantID string, key sideeffect.Key, a *actionRow) error {
	_, err := tx.ExecContext(ctx,
		`UPDATE gak_effect_action
		    SET done = ?, result = ?, uncertain = ?, reason = ?,
		        intent_resource = ?, intent_fence = ?, intent_holder = ?, intent_expires = ?,
		        recon_holder = ?, recon_expires = ?
		  WHERE tenant = ? AND key = ?`,
		boolInt(a.done), nullBytes(a.result, a.hasResult), boolInt(a.uncertain), a.reason,
		nullString(a.intentResource), nullInt(a.intentFence), nullString(a.intentHolder), nullInt(a.intentExpires),
		nullString(a.reconHolder), nullInt(a.reconExpires),
		tenantID, string(key))
	if err != nil {
		return errors.Wrap(err, "sqliteadapter: 写动作行失败", "key", string(key))
	}
	return nil
}

// boolInt bool → SQLite 的 INTEGER（0/1）。
func boolInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// nullString 可空文本列的写值（未登记 ⇒ NULL）。
func nullString(v sql.NullString) any {
	if v.Valid {
		return v.String
	}
	return nil
}

// nullInt 可空整数列的写值（未登记 ⇒ NULL）。
func nullInt(v sql.NullInt64) any {
	if v.Valid {
		return v.Int64
	}
	return nil
}

// nullStringValue 空的可空文本列（资源闸被放掉时用）。
func nullStringValue() sql.NullString { return sql.NullString{} }

// nullInt64Value 带值的可空整数列。
func nullInt64Value(v int64) sql.NullInt64 { return sql.NullInt64{Int64: v, Valid: true} }

// nullBytes 可空二进制列的写值（无结果 ⇒ NULL；**已记录但载荷为空** ⇒ 空 BLOB，
// 不是 NULL——「做过但结果为空」与「没做过」是两件事）。
func nullBytes(b []byte, has bool) any {
	if !has {
		return nil
	}
	if b == nil {
		return []byte{}
	}
	return b
}
