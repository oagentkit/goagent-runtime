package pgstore

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/oekit/pkg/errors"

	"github.com/oagentkit/goagent-kernel/sideeffect"
)

// 本文件钉住 #223：执行意图四列**同存同缺**，半写行 fail-closed。
//
// 触发形态（issue 原文）：`intent_fence` 有值、`intent_resource` 为 NULL。
// 旧实现 hasIntent() 只判 intentFence，于是 Acquire 在**行锁持有、事务开着**
// 的路径上解引用 NULL —— 库层 panic 直接杀宿主进程。修法有两层：
//   - hasIntent / checkIntentColumns 按四列口径判（全有才是意图）；
//   - lockAction 在读到行时就把半写行拒成 ErrIntentIncomplete（所有入口
//     共用这一处，不是只给 Acquire 打补丁）。

// TestActionRowIntentColumnsAllOrNone 穷举 16 种列填充组合（确定性、无数据库）。
//
// 只测「半写会报错」不够：三个正常形态（全空 / 全有）必须放行，否则
// 判据从「漏判」变成「误拒」，同样不可用。
func TestActionRowIntentColumnsAllOrNone(t *testing.T) {
	res, holder := "res-a", "holder-a"
	fence := int64(7)
	expires := time.Unix(1_700_000_000, 0).UTC()

	for mask := 0; mask < 16; mask++ {
		var row actionRow
		if mask&1 != 0 {
			row.intentResource = &res
		}
		if mask&2 != 0 {
			row.intentFence = &fence
		}
		if mask&4 != 0 {
			row.intentHolder = &holder
		}
		if mask&8 != 0 {
			row.intentExpires = &expires
		}

		t.Run(fmt.Sprintf("cols=%04b", mask), func(t *testing.T) {
			err := row.checkIntentColumns()
			switch mask {
			case 0: // 四列全空 = 没有未裁决的尝试（正常形态）
				if err != nil {
					t.Fatalf("全空行不得被拒: %v", err)
				}
				if row.hasIntent() {
					t.Fatal("全空行不得报告有意图")
				}
			case 15: // 四列全有 = 可判定的意图（正常形态）
				if err != nil {
					t.Fatalf("四列齐全不得被拒: %v", err)
				}
				if !row.hasIntent() {
					t.Fatal("四列齐全应报告有意图")
				}
			default: // 其余 14 种半写组合一律 fail-closed
				if !errors.Is(err, ErrIntentIncomplete) {
					t.Fatalf("半写行必须返回 ErrIntentIncomplete，实得 %v", err)
				}
				if row.hasIntent() {
					t.Fatal("半写行不得报告有意图（否则会被当空行放行）")
				}
			}
		})
	}
}

// TestAcquireFailsClosedOnPartialIntentRow 在真实 PostgreSQL 上钉 Acquire 的
// 行为：半写行返回明确错误（不 panic），且整笔事务回滚——资源行也不落。
//
// 没有可连的 PostgreSQL 时在**环境入口**显式跳过（同 pg_test.go 的口径）；
// CI 的 integration job 必跑本用例。
func TestAcquireFailsClosedOnPartialIntentRow(t *testing.T) {
	dsn := requireDSN(t)
	eff, _, _, _ := freshStores(t, dsn)
	ctx := context.Background()

	cases := []struct {
		name     string
		key      string
		resource string
		insert   string
		complete bool
	}{
		{
			name:     "fence 有值 resource 为 NULL（#223 触发形态）",
			key:      "k-partial-fence",
			resource: "res-partial-fence",
			insert:   `INSERT INTO pgref_effect_action (key, intent_fence) VALUES ($1, 7)`,
		},
		{
			name:     "resource/fence/expires 齐全但 holder 为 NULL（四列口径）",
			key:      "k-partial-holder",
			resource: "res-partial-holder",
			insert: `INSERT INTO pgref_effect_action (key, intent_resource, intent_fence, intent_expires)
			         VALUES ($1, 'res-elsewhere', 7, clock_timestamp() + interval '1 hour')`,
		},
		{
			name:     "只有 resource（fence/expires 为 NULL）",
			key:      "k-partial-resource",
			resource: "res-partial-resource",
			insert:   `INSERT INTO pgref_effect_action (key, intent_resource) VALUES ($1, 'res-elsewhere')`,
		},
		{
			name:     "四列齐全（负对照：不得被误拒）",
			key:      "k-complete-intent",
			resource: "res-complete-intent",
			insert: `INSERT INTO pgref_effect_action (key, intent_resource, intent_fence, intent_holder, intent_expires)
			         VALUES ($1, 'res-complete-intent', 7, 'holder-a', clock_timestamp() + interval '1 hour')`,
			complete: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := eff.pool.Exec(ctx, tc.insert, tc.key); err != nil {
				t.Fatalf("构造行失败: %v", err)
			}
			outcome, _, _, err := eff.Acquire(ctx, sideeffect.Action{
				Key: sideeffect.Key(tc.key), Resource: tc.resource, Payload: []byte("p"),
			}, time.Minute)

			if tc.complete {
				if err != nil {
					t.Fatalf("四列齐全的意图行不得被拒: %v", err)
				}
				if outcome != sideeffect.OutcomeContended {
					t.Fatalf("未过期的完整意图应判 contended，实得 %v", outcome)
				}
				return
			}
			if !errors.Is(err, ErrIntentIncomplete) {
				t.Fatalf("半写意图行必须 fail-closed 返回 ErrIntentIncomplete，实得 %v", err)
			}
			// 整体回滚：拒绝发生在任何写入之前，资源行也不得留下。
			var n int
			if qerr := eff.pool.QueryRow(ctx,
				`SELECT count(*) FROM pgref_effect_resource WHERE resource = $1`, tc.resource).Scan(&n); qerr != nil {
				t.Fatalf("查资源行失败: %v", qerr)
			}
			if n != 0 {
				t.Fatalf("拒绝的判定不得留下资源行（实得 %d 行）", n)
			}
		})
	}
}
