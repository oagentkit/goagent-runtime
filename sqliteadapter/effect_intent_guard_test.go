// effect_intent_guard_test.go —— 半写行 fail-closed 守卫（ErrIntentIncomplete）的判据。
//
// 对位 pgstore（v0.2.0 自 goagent-kernel 迁入）/pg_intent_guard_test.go：同一套判据、两种方言
// （PG 侧列用指针，SQLite 侧用 sql.Null* 显式区分 NULL 与零值）。
//
// 本文件钉住的是「执行意图四列同存同空」这条**存储侧不变量**：
// 四列要么全有、要么全空；只填一部分（1~3 列有值）= 上次执行意图写到一半
// 就崩了 ⇒ 判定必须 fail-closed（ErrIntentIncomplete），**不得**当成
// 「没有意图」放行——放行等于重新授予执行权，重复施加的入口重新打开。
//
// 为什么要两条用例（各管一段，缺一条就有洞）：
//
//   - 穷举 16 种填充组合（确定性、无数据库）：钉住「全空/全满放行 + 其余 14 种
//     拒绝」。只测「半写会报错」不够——正常形态被误拒同样不可用。
//   - 真库半写行 ⇒ Acquire 明确报错且资源行不落：钉住**接线**。守卫本身写对了
//     但没接进判定路径（lockAction 不调它）时，第一条照样绿。
package sqliteadapter

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/oekit/pkg/errors"

	"github.com/yi-nology/goagent-kernel/sideeffect"
)

// TestActionRowIntentColumnsAllOrNone 穷举 16 种列填充组合（确定性、无数据库）。
//
// 对位 PG 侧同名用例：掩码位序（resource=1 / fence=2 / holder=4 / expires=8）
// 与判据逐条照搬，只是列的载体从指针换成 sql.Null*。
func TestActionRowIntentColumnsAllOrNone(t *testing.T) {
	res, holder := "res-a", "holder-a"
	fence := int64(7)
	expires := time.Unix(1_700_000_000, 0).UTC()

	for mask := 0; mask < 16; mask++ {
		var row actionRow
		if mask&1 != 0 {
			row.intentResource = sql.NullString{String: res, Valid: true}
		}
		if mask&2 != 0 {
			row.intentFence = sql.NullInt64{Int64: fence, Valid: true}
		}
		if mask&4 != 0 {
			row.intentHolder = sql.NullString{String: holder, Valid: true}
		}
		if mask&8 != 0 {
			row.intentExpires = sql.NullInt64{Int64: expires.UnixMilli(), Valid: true}
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

// TestAcquireFailsClosedOnPartialIntentRow 在真实 SQLite 库上钉 Acquire 的行为：
// 半写行返回明确错误（不 panic、不静默成功），且整笔事务回滚——资源行也不落。
//
// 半写行**绕过存储自己的写路径**直接 INSERT：存储只写「四列全有」或「四列全空」，
// 自己写不出半写行 ⇒ 只能模拟「写到一半崩了」的仓外写入方（人工订正 / schema
// 迁移 / 仓外写入方）。
func TestAcquireFailsClosedOnPartialIntentRow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s := openEffectStore(t)
	e := s.Effects()

	// 存储时钟（unixepoch('subsec')）口径的「一小时后」：完整意图行必须未过期。
	expiresAt := time.Now().Add(time.Hour).UnixMilli()

	cases := []struct {
		name     string
		key      string
		resource string
		insert   string
		args     []any
		complete bool
	}{
		{
			name:     "fence 有值 resource 为 NULL",
			key:      "k-partial-fence",
			resource: "res-partial-fence",
			insert:   `INSERT INTO gak_effect_action (tenant, key, intent_fence) VALUES ('', ?, 7)`,
			args:     []any{"k-partial-fence"},
		},
		{
			name:     "resource/fence/expires 齐全但 holder 为 NULL（四列口径）",
			key:      "k-partial-holder",
			resource: "res-partial-holder",
			insert: `INSERT INTO gak_effect_action (tenant, key, intent_resource, intent_fence, intent_expires)
			         VALUES ('', ?, 'res-elsewhere', 7, ?)`,
			args: []any{"k-partial-holder", expiresAt},
		},
		{
			name:     "只有 resource（fence/expires 为 NULL）",
			key:      "k-partial-resource",
			resource: "res-partial-resource",
			insert:   `INSERT INTO gak_effect_action (tenant, key, intent_resource) VALUES ('', ?, 'res-elsewhere')`,
			args:     []any{"k-partial-resource"},
		},
		{
			name:     "四列齐全（负对照：不得被误拒）",
			key:      "k-complete-intent",
			resource: "res-complete-intent",
			insert: `INSERT INTO gak_effect_action (tenant, key, intent_resource, intent_fence, intent_holder, intent_expires)
			         VALUES ('', ?, 'res-complete-intent', 7, 'holder-a', ?)`,
			args:     []any{"k-complete-intent", expiresAt},
			complete: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := s.DB().ExecContext(ctx, tc.insert, tc.args...); err != nil {
				t.Fatalf("构造半写行失败: %v", err)
			}
			outcome, _, _, err := e.Acquire(ctx, sideeffect.Action{
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
			if qerr := s.DB().QueryRowContext(ctx,
				`SELECT count(*) FROM gak_effect_resource WHERE tenant = '' AND resource = ?`, tc.resource).Scan(&n); qerr != nil {
				t.Fatalf("查资源行失败: %v", qerr)
			}
			if n != 0 {
				t.Fatalf("拒绝的判定不得留下资源行（实得 %d 行）", n)
			}
		})
	}
}
