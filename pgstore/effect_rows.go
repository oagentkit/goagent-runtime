// effect_rows.go —— effect 存储的行级读写与凭据联查（从 effect.go 拆出，
// 只为守住「单文件 400 行」的结构阈值；判据归属不变）。
package pgstore

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/oekit/pkg/errors"

	"github.com/oagentkit/goagent-kernel/sideeffect"
)

// ensureActionRow / ensureResourceRow 保证行存在（Acquire 的自增主键语义：
// 「没做过」「没人占」就是空行——首个动作顺手把行建出来）。
func ensureActionRow(ctx context.Context, tx pgx.Tx, act sideeffect.Action) error {
	// INSERT 同时登记动作身份（首次见到这个 Key 的调用就是身份登记时刻，
	// 即便这次 Acquire 的结果是 contended——身份不等授予）。冲突（已存在）
	// 不更新：身份以先到者的登记为准，后到的不一致者由 lockAction 内的
	// ensureIdentity 拒掉（ErrActionConflict）。
	_, err := tx.Exec(ctx,
		`INSERT INTO pgref_effect_action (tenant, key, id_resource, id_safety, id_payload_hash)
		VALUES ($1, $2, $3, $4, $5) ON CONFLICT (tenant, key) DO NOTHING`,
		act.Tenant, string(act.Key), act.Resource, int16(act.RetrySafety), sideeffect.HashPayload(act.Payload))
	if err != nil {
		return errors.Wrap(err, "pgstore: 登记动作行失败", "key", string(act.Key))
	}
	return nil
}

// ensureIdentity 动作身份校验（调用方持行锁）：未登记 → 补登记（老数据
// 行的兼容路径）；已登记 → 与本次 act 比对，不一致即 ErrActionConflict。
//
// 比对在 lockAction 内完成 ⇒ 每个入口拿到行的那一刻就先问身份，
// 「已完成 → 重放」等任何判定都在它之后——把旧身份的结果交给新身份的
// 调用方是答非所问（Store.Acquire 契约「身份不可变」）。
func ensureIdentity(ctx context.Context, tx pgx.Tx, a *actionRow, act sideeffect.Action) error {
	if a.idResource == nil || a.idSafety == nil || a.idPayloadHash == nil {
		res := act.Resource
		safety := int16(act.RetrySafety)
		hash := sideeffect.HashPayload(act.Payload)
		if _, err := tx.Exec(ctx,
			`UPDATE pgref_effect_action SET id_resource = $3, id_safety = $4, id_payload_hash = $5
			  WHERE tenant = $1 AND key = $2 AND id_resource IS NULL`,
			act.Tenant, string(act.Key), res, safety, hash); err != nil {
			return errors.Wrap(err, "pgstore: 登记动作身份失败", "key", string(act.Key))
		}
		a.idResource, a.idSafety, a.idPayloadHash = &res, &safety, &hash
		return nil
	}
	if *a.idResource != act.Resource || int16(act.RetrySafety) != *a.idSafety ||
		*a.idPayloadHash != sideeffect.HashPayload(act.Payload) {
		return errors.Wrap(sideeffect.ErrActionConflict,
			"同 Key 的动作身份与首次登记不一致（这不是重试，是另一个动作）",
			"key", string(act.Key),
			"登记 resource", *a.idResource, "本次 resource", act.Resource,
			"登记 safety", sideeffect.RetrySafety(*a.idSafety), "本次 safety", act.RetrySafety)
	}
	return nil
}

func ensureResourceRow(ctx context.Context, tx pgx.Tx, tenant, resource string) error {
	_, err := tx.Exec(ctx,
		`INSERT INTO pgref_effect_resource (tenant, resource) VALUES ($1, $2)
		 ON CONFLICT (tenant, resource) DO NOTHING`,
		tenant, resource)
	if err != nil {
		return errors.Wrap(err, "pgstore: 登记资源行失败", "resource", resource)
	}
	return nil
}

// actionRow Key 级状态的一行（列语义见 schema.go 的表注释）。
type actionRow struct {
	done      bool
	result    *[]byte
	uncertain bool
	reason    string

	// 动作身份（首次登记，永久事实；NULL/nil = 未登记 = 老数据行，
	// 由 ensureIdentity 首见时补登记）。三者与 ErrActionConflict 对应。
	idResource    *string
	idSafety      *int16
	idPayloadHash *string

	intentResource *string
	intentFence    *int64
	intentHolder   *string
	intentExpires  *time.Time

	reconHolder  *string
	reconExpires *time.Time
}

// ErrIntentIncomplete 动作行的执行意图四列**部分填充**（半写行）。
//
// 这是存储侧数据损坏 / 仓外写入方只写部分列的形态，不是本包写入路径能
// 产生的状态（本包只写「四列全有」或「四列全空」）。判定遇到它必须
// **fail-closed**：逐列解引用会 panic（库层 panic 直接杀宿主进程），
// 按「无意图」放行会重新授予执行权（重复施加的入口重新打开）。
var ErrIntentIncomplete = errors.New("pgstore: 执行意图列部分填充，拒绝判定")

// intentColumnsFilled 执行意图四列中非 NULL 的列数。
func (r *actionRow) intentColumnsFilled() int {
	n := 0
	if r.intentResource != nil {
		n++
	}
	if r.intentFence != nil {
		n++
	}
	if r.intentHolder != nil {
		n++
	}
	if r.intentExpires != nil {
		n++
	}
	return n
}

// checkIntentColumns 半写行 fail-closed：既不解引用 NULL，也不当空行放行。
// 四列全空是正常形态（没有未裁决的尝试）；全有才是可判定的意图。
func (r *actionRow) checkIntentColumns() error {
	if n := r.intentColumnsFilled(); n != 0 && n != 4 {
		return errors.Wrap(ErrIntentIncomplete, "执行意图四列未同存同缺（半写行）——拒绝判定", "filled", n)
	}
	return nil
}

// hasIntent 该 Key 上是否挂着一次未裁决的尝试（四列同时判）。
//
// 只判 intentFence 会让「fence 有值、resource/expires 为 NULL」的半写行
// 进入解引用路径（panic）或漏判成「无意图」（重新授予执行权）。半写行由
// lockAction 的 checkIntentColumns 显式拒绝，故这里只需回答「四列齐全」。
func (r *actionRow) hasIntent() bool { return r.intentColumnsFilled() == 4 }

// hasRecon 该 Key 上是否挂着对账占用。
func (r *actionRow) hasRecon() bool { return r.reconExpires != nil }

// lockAction 锁住并读取 Key 级状态行（调用前必须 ensureActionRow）。
// lockAction 锁定并读出动作行，**锁后立刻校验动作身份**（见 ensureIdentity）
// ——所有入口共用这一处，身份判定天然先于任何状态判定。
func lockAction(ctx context.Context, tx pgx.Tx, act sideeffect.Action) (*actionRow, error) {
	row := tx.QueryRow(ctx,
		`SELECT done, result, uncertain, reason,
		        id_resource, id_safety, id_payload_hash,
		        intent_resource, intent_fence, intent_holder, intent_expires,
		        recon_holder, recon_expires
		   FROM pgref_effect_action WHERE tenant = $1 AND key = $2 FOR UPDATE`,
		act.Tenant, string(act.Key))
	var r actionRow
	if err := row.Scan(&r.done, &r.result, &r.uncertain, &r.reason,
		&r.idResource, &r.idSafety, &r.idPayloadHash,
		&r.intentResource, &r.intentFence, &r.intentHolder, &r.intentExpires,
		&r.reconHolder, &r.reconExpires); err != nil {
		return nil, errors.Wrap(err, "pgstore: 读动作行失败", "key", string(act.Key))
	}
	// 半写意图行在这里就 fail-closed（所有入口共用 lockAction，漏不掉）：
	// 继续判下去要么解引用 NULL panics，要么把半写行当「无意图」放行。
	if err := r.checkIntentColumns(); err != nil {
		return nil, errors.Wrap(err, "pgstore: 动作行不可判定", "key", string(act.Key))
	}
	if err := ensureIdentity(ctx, tx, &r, act); err != nil {
		return nil, err
	}
	return &r, nil
}

// resourceRow 资源级状态的一行。
type resourceRow struct {
	fence  int64
	holder *string
	until  *time.Time
}

// held 该资源当前是否被占着且租约未过期（零值 holder = 空闲）。
func (r *resourceRow) held(now time.Time) bool {
	return r.holder != nil && r.until != nil && r.until.After(now)
}

// lockResource 锁住并读取资源级状态行（调用前必须 ensureResourceRow）。
func lockResource(ctx context.Context, tx pgx.Tx, tenant, resource string) (*resourceRow, error) {
	row := tx.QueryRow(ctx,
		`SELECT fence, holder, until FROM pgref_effect_resource
		  WHERE tenant = $1 AND resource = $2 FOR UPDATE`,
		tenant, resource)
	var r resourceRow
	if err := row.Scan(&r.fence, &r.holder, &r.until); err != nil {
		return nil, errors.Wrap(err, "pgstore: 读资源行失败", "resource", resource)
	}
	return &r, nil
}

// leaseResourceErr 三项联查的第一项：租约的资源必须与动作的资源一致。
// （与内存版同一段判据：A 资源的租约被拿去做 B 资源的操作时，两份真相
// 拼出一个错误的「合规」。）
func leaseResourceErr(act sideeffect.Action, lease sideeffect.Lease) error {
	if lease.Resource != act.Resource {
		return errors.Wrap(sideeffect.ErrResourceMismatch,
			"凭据被拒：租约与动作的资源不一致",
			"act_resource", act.Resource, "lease_resource", lease.Resource)
	}
	return nil
}

// holdOwnerErr 三项联查：资源闸是否仍由**这份凭据**持有（行已锁，直接比）。
//
// 判定顺序与内存版一致：令牌不匹配或已释放 → ErrFenceStale；
// 令牌还在这一代但持有者不是出示者 → ErrNotHolder。
func holdOwnerErr(act sideeffect.Action, lease sideeffect.Lease, rr *resourceRow) error {
	if err := leaseResourceErr(act, lease); err != nil {
		return err
	}
	if lease.Fence == 0 || rr.holder == nil || rr.fence != int64(lease.Fence) {
		return errors.Wrap(sideeffect.ErrFenceStale,
			"凭据被拒：本次持有已失效（令牌不是最新代或资源已释放）",
			"key", string(act.Key), "resource", act.Resource)
	}
	if *rr.holder != lease.Holder {
		return errors.Wrap(sideeffect.ErrNotHolder,
			"凭据被拒：持有者身份不匹配（令牌不是本次授予铸造的那份）",
			"key", string(act.Key), "resource", act.Resource)
	}
	return nil
}

// intentOwnerErr 三项联查：本次尝试是否仍归**这份凭据**所有。
func intentOwnerErr(act sideeffect.Action, lease sideeffect.Lease, a *actionRow) error {
	if err := leaseResourceErr(act, lease); err != nil {
		return err
	}
	if !a.hasIntent() {
		return errors.Wrap(sideeffect.ErrFenceStale,
			"凭据被拒：本次尝试已无未裁决的执行意图",
			"key", string(act.Key))
	}
	if *a.intentFence != int64(lease.Fence) {
		return errors.Wrap(sideeffect.ErrFenceStale,
			"凭据被拒：本次尝试已不属于该令牌",
			"key", string(act.Key), "resource", act.Resource)
	}
	if a.intentHolder == nil || *a.intentHolder != lease.Holder {
		return errors.Wrap(sideeffect.ErrNotHolder,
			"凭据被拒：持有者身份不匹配（意图不是本次授予铸造的那份）",
			"key", string(act.Key))
	}
	return nil
}
