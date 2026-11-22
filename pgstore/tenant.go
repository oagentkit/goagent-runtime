// tenant.go —— 租户作用域的装配期声明与解析（issue #275）。
//
// # 两种形态，一个判据入口
//
//   - **默认（旧布局）**：构造器 NewEffectStore / NewEventStore / NewDownstream
//     不声明租户作用域，租户列取动作 / 事件自带的 Tenant（空 = ”）。既有
//     单租户部署逐字不变；宿主显式给了 Action.Tenant / Event.Tenant 时，
//     存储仍按该值分区（不会静默并进 ” 流）。
//   - **租户作用域**：NewTenantEffectStore / NewTenantEventStore /
//     NewTenantDownstream 在装配期声明（与 coord.NewTenantBoardAdapter
//     同款）。此后每次调用都从 ctx 取租户：缺租户 ⇒ tenant.ErrNoContext
//     （fail-closed，**不是**「不限」）；动作 / 事件自带的 Tenant 若与
//     ctx 不一致 ⇒ ErrTenantMismatch（同一份写入不能有两个租户结论）。
//
// 解析只发生在**入口**：公共方法先把作用域解析成具体租户，再交给 SQL。
// 下游 SQL 一律 (tenant, 业务键) 联合定位，不存在「先按键查、再补租户
// 过滤」的两步窗口。
package pgstore

import (
	"context"

	"github.com/oekit/pkg/errors"

	"github.com/yi-nology/goagent-kernel/tenant"
)

// ErrTenantMismatch 声明的租户与上下文租户不一致（同一次操作两个租户结论）。
//
// 处置：修声明。调用方要么让 Action.Tenant / Event.Tenant 与
// tenant.Context.Tenant 一致，要么不要同时给两份；存储拒绝猜测哪一份是
// 对的——猜错的方向是跨租户串数据。
var ErrTenantMismatch = errors.New("pgstore: 声明的租户与上下文租户不一致")

// resolveTenant 解析本次操作的存储租户。
//
// scoped=false：不强制租户，返回 declared（空 = 旧布局 / 单租户命名空间）。
// scoped=true：ctx 必须带租户（tenant.Require，缺 ⇒ tenant.ErrNoContext）；
// declared 非空时必须与 ctx 一致。
func resolveTenant(ctx context.Context, scoped bool, declared string) (string, error) {
	if !scoped {
		return declared, nil
	}
	c, err := tenant.Require(ctx)
	if err != nil {
		return "", err
	}
	if declared != "" && declared != c.Tenant {
		return "", errors.Wrap(ErrTenantMismatch,
			"写入声明的租户与上下文租户不一致（拒绝猜测）",
			"declared", declared, "context", c.Tenant)
	}
	return c.Tenant, nil
}
