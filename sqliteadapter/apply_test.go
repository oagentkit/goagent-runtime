// apply_test.go —— 跨面 Apply 的判据：同一次提交 + 失败即作废整批。
//
// # 为什么要有 Apply
//
// 本文件覆盖的这几张面（任务 / 副作用 / 快照 / 事件 / 下游栅栏）各自提交 ⇒
// 「任务做完了但没留痕」「留痕了但没做完」两类半截状态结构性地可能。
// Apply 把各面的变更收进**同一次提交**，失败即作废整批
// （即使调用方吞掉了内层的错误）。
//
// # 三条最容易踩的坑（用例逐条钉住）
//
//  1. **嵌套事务会死锁**（表现为 ctx 超时，不是干净报错）：写句柄只有一条
//     连接 ⇒ 各面各开事务 ⇒ 第二个面等第一个面的连接 ⇒ 永久等。
//     ⇒ 用例全部走 tx.*() 的事务内形态，且 ctx 带短超时（回归要**快速红**）。
//  2. **失败槽的覆盖面**：只有经 exec.run 的写路径才会被记录 ⇒ 逐面核
//     （TestApplyFailureSlotCoversEachFace 逐面触发一次失败）。
//  3. **失败被吞掉也要作废整批**：内层错误被调用方吞掉时，整批仍必须回滚。
package sqliteadapter

import (
	"context"
	"testing"
	"time"

	"github.com/oekit/pkg/errors"

	"github.com/oagentkit/goagent-kernel/checkpoint"
	"github.com/oagentkit/goagent-kernel/eventbus"
	"github.com/oagentkit/goagent-kernel/sideeffect"
	"github.com/oagentkit/goagent-kernel/taskboard"
)

// appCtx 用例的 ctx（带超时：回归要**快速红**，而不是挂在 60s 超时上）。
//
// 超时取 5s：Apply 内部的死锁（嵌套事务）会在这里快速变红，而不是挂死。
func appCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// TestApplyCommitsAllFacesAtomically 本用例覆盖的这几张面（任务 / 副作用 /
// 快照 / 事件 / 下游栅栏）的变更落进**同一次提交**。
//
// 判据：提交后这几张面都读得到（一次提交 ⇒ 全部生效），且任一面失败则全部
// 不生效（见下面两条用例）。
func TestApplyCommitsAllFacesAtomically(t *testing.T) {
	ctx := appCtx(t)
	s := openStore(t)
	s.Board().Seed(taskboard.Task{ID: "t1", State: taskboard.StateOpen})

	err := s.Apply(ctx, func(tx *Tx) error {
		// 任务面：认领（读-判-写同事务）。
		if _, err := tx.Tasks().Claim(ctx, "t1", "agent-a", taskboard.ClaimRequest{TTL: time.Minute}); err != nil {
			return err
		}
		// 副作用面：授予执行权。
		act := sideeffect.Action{Key: "k-apply", Resource: "res-apply"}
		if _, _, _, err := tx.Effects().Acquire(ctx, act, time.Minute); err != nil {
			return err
		}
		// 快照面：落一条进度快照。
		if err := tx.Snapshots().SaveIfNewer(ctx, checkpoint.Snapshot{
			RunID: "run-apply", Seq: 1, Status: "running",
		}); err != nil {
			return err
		}
		// 事件面：发布一条事件。
		if _, err := tx.Events().Insert(ctx, eventbus.Event{SessionID: "s-apply", Type: "t-apply"}); err != nil {
			return err
		}
		// 栅栏面：批内预占 + 写入体（写入体走同一事务 ⇒ 不得部分写）。
		if err := tx.Downstream().WriteFenced(ctx, "res-apply", 1, func(ctx context.Context) error {
			return tx.Downstream().WriteMarker(ctx, "res-apply")
		}); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Apply 失败: %v", err)
	}

	// 本用例覆盖的这几张面都读得到（⇒ 变更同生共死）。
	tasks, err := s.Board().List(ctx)
	if err != nil {
		t.Fatalf("读任务失败: %v", err)
	}
	if len(tasks) != 1 || tasks[0].State != taskboard.StateClaimed {
		t.Fatalf("任务面未随批提交：%+v", tasks)
	}
	snap, err := s.Snapshots().Load(ctx, "run-apply")
	if err != nil {
		t.Fatalf("读快照失败（快照面未随批提交）: %v", err)
	}
	if snap.Seq != 1 {
		t.Fatalf("快照序号应为 1，实得 %d", snap.Seq)
	}
	wm, err := s.Events().CommittedWatermark(ctx)
	if err != nil {
		t.Fatalf("读水位线失败: %v", err)
	}
	if wm != 1 {
		t.Fatalf("事件面未随批提交（水位线应为 1），实得 %d", wm)
	}
	var n int
	if err := s.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM gak_effect_action WHERE key = 'k-apply'`).Scan(&n); err != nil {
		t.Fatalf("读动作行数失败: %v", err)
	}
	if n != 1 {
		t.Fatalf("副作用面未随批提交，实得 %d 行", n)
	}
	marker, err := s.Downstream().Marker(ctx, "res-apply")
	if err != nil {
		t.Fatalf("读栅栏标记失败: %v", err)
	}
	if marker != "m" {
		t.Fatalf("栅栏面未随批提交（业务标记应为 m），实得 %q", marker)
	}
}

// TestApplyRollsBackEverythingOnError 回调返回错误 ⇒ 整批丢弃。
func TestApplyRollsBackEverythingOnError(t *testing.T) {
	ctx := appCtx(t)
	s := openStore(t)

	sentinel := errors.New("故意失败")
	err := s.Apply(ctx, func(tx *Tx) error {
		if _, err := tx.Events().Insert(ctx, eventbus.Event{SessionID: "s", Type: "t"}); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("Apply 必须原样传出回调的错误: %v", err)
	}
	wm, err := s.Events().CommittedWatermark(ctx)
	if err != nil {
		t.Fatalf("读水位线失败: %v", err)
	}
	if wm != 0 {
		t.Fatalf("回滚后水位线必须为 0（事件行不得留下）: %d", wm)
	}
}

// TestApplyReturnsCallbackErrorVerbatim 回调的错误**原样**传出（不被包装）。
//
// 判据：错误身份必须保持（errors.Is 之外还断等值）⇒ 调用方能按自己的哨兵
// 分流。
func TestApplyReturnsCallbackErrorVerbatim(t *testing.T) {
	ctx := appCtx(t)
	s := openStore(t)

	sentinel := errors.New("故意失败")
	err := s.Apply(ctx, func(tx *Tx) error { return sentinel })
	if err != sentinel {
		t.Fatalf("Apply 必须原样传出回调的错误（不得包装），实得 %v", err)
	}
}

// TestApplyVoidsBatchWhenInnerFailureSwallowed 内层错误被吞掉 ⇒ 整批仍作废。
//
// 这是「失败即作废整批」的**承重**判据：调用方吞掉错误后，若批次仍提交，
// 就会出现「一半已提交」——正是本包要杜绝的形态。
func TestApplyVoidsBatchWhenInnerFailureSwallowed(t *testing.T) {
	ctx := appCtx(t)
	s := openStore(t)
	s.Board().Seed(taskboard.Task{ID: "t1", State: taskboard.StateOpen})

	err := s.Apply(ctx, func(tx *Tx) error {
		// 内层失败：令牌不匹配的完成必被拒。**故意吞掉**这个错误继续写。
		if cerr := tx.Tasks().Complete(ctx, "t1", "no-such-token"); cerr == nil {
			return errors.New("令牌不匹配的完成必须被拒")
		}
		// 继续写别的面：这些写也必须随整批一起消失。
		if _, ierr := tx.Events().Insert(ctx, eventbus.Event{SessionID: "s", Type: "t"}); ierr != nil {
			return ierr
		}
		return nil
	})
	// 失败被吞掉时，Apply 仍须把「本批已失败」这件事报出来（否则调用方以为
	// 整批提交了，而其实什么都没写 ⇒ 静默的半截状态）。
	if err == nil {
		t.Fatal("内层失败被吞掉时 Apply 不得报成功（本批已作废）")
	}
	if !errors.Is(err, taskboard.ErrLeaseLost) {
		t.Fatalf("Apply 报出的应是内层那条失败（taskboard.ErrLeaseLost），实得 %v", err)
	}

	// 关键：整批回滚 ⇒ 事件行不得留下。
	wm, werr := s.Events().CommittedWatermark(ctx)
	if werr != nil {
		t.Fatalf("读水位线失败: %v", werr)
	}
	if wm != 0 {
		t.Fatalf("失败被吞掉后整批仍必须回滚（水位线应为 0），实得 %d", wm)
	}
	// 任务状态未变（仍是 open）。
	tasks, lerr := s.Board().List(ctx)
	if lerr != nil {
		t.Fatalf("读任务失败: %v", lerr)
	}
	if len(tasks) != 1 || tasks[0].State != taskboard.StateOpen {
		t.Fatalf("失败被吞掉后任务状态不得被提交（半截事务），实得 %+v", tasks)
	}
}

// TestApplySharesOneTransactionAcrossFaces 各面共用**同一个**事务。
//
// 判据：事务内的读看得见事务内的未提交写（读走 tx，不走只读句柄）⇒ 只有
// 同一个 tx 才成立。分面各开事务时这条判据会退化成「读不到」或挂死。
func TestApplySharesOneTransactionAcrossFaces(t *testing.T) {
	ctx := appCtx(t)
	s := openStore(t)

	err := s.Apply(ctx, func(tx *Tx) error {
		if err := tx.Snapshots().SaveIfNewer(ctx, checkpoint.Snapshot{
			RunID: "run-shared", Seq: 1, Status: "running",
		}); err != nil {
			return err
		}
		// 同一事务内读：走 tx ⇒ 看得见本事务刚写的未提交行。
		snap, err := tx.Snapshots().Load(ctx, "run-shared")
		if err != nil {
			return errors.Wrap(err, "事务内读不到本事务刚写的行（各面没有共用同一个 tx）")
		}
		if snap.Seq != 1 {
			return errors.New("事务内读到的快照序号不对")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Apply 失败（各面没有共用同一个事务）: %v", err)
	}
}

// TestApplyFailureSlotCoversEachFace 失败槽的覆盖面：**逐面**核。
//
// 每个面各触发一次失败（错误被吞掉），断言整批仍被作废 ⇒ 该面的写路径确实
// 经由失败槽登记。少了任何一面的登记，「失败即作废整批」对那一面就是假的。
func TestApplyFailureSlotCoversEachFace(t *testing.T) {
	cases := []struct {
		name string
		// fail 在批内触发一次失败（返回的错误会被用例吞掉）。
		fail func(ctx context.Context, tx *Tx) error
		// write 批内的一次成功写（用于证明整批被作废）。
		write func(ctx context.Context, tx *Tx) error
		// assertVoided 批外读：断言批内的写没有留下。
		assertVoided func(t *testing.T, ctx context.Context, s *Store)
	}{
		{
			name: "任务面",
			fail: func(ctx context.Context, tx *Tx) error {
				return tx.Tasks().Complete(ctx, "t1", "no-such-token")
			},
			write: func(ctx context.Context, tx *Tx) error {
				_, err := tx.Tasks().Claim(ctx, "t1", "agent-a", taskboard.ClaimRequest{TTL: time.Minute})
				return err
			},
			assertVoided: func(t *testing.T, ctx context.Context, s *Store) {
				t.Helper()
				tasks, err := s.Board().List(ctx)
				if err != nil {
					t.Fatalf("读任务失败: %v", err)
				}
				if len(tasks) != 1 || tasks[0].State != taskboard.StateOpen {
					t.Fatalf("任务面未登记失败 ⇒ 整批仍提交了（半截事务），实得 %+v", tasks)
				}
			},
		},
		{
			name: "副作用面",
			fail: func(ctx context.Context, tx *Tx) error {
				// 先在同一批里登记身份（成功），再用不一致的资源 ⇒ 身份冲突。
				if _, _, _, err := tx.Effects().Acquire(ctx,
					sideeffect.Action{Key: "k-conflict", Resource: "res-a"}, time.Minute); err != nil {
					return err
				}
				_, _, _, err := tx.Effects().Acquire(ctx,
					sideeffect.Action{Key: "k-conflict", Resource: "res-b"}, time.Minute)
				return err
			},
			write: func(ctx context.Context, tx *Tx) error {
				_, _, _, err := tx.Effects().Acquire(ctx,
					sideeffect.Action{Key: "k-other", Resource: "res-c"}, time.Minute)
				return err
			},
			assertVoided: func(t *testing.T, ctx context.Context, s *Store) {
				t.Helper()
				var n int
				if err := s.DB().QueryRowContext(ctx,
					`SELECT COUNT(*) FROM gak_effect_action`).Scan(&n); err != nil {
					t.Fatalf("读动作行数失败: %v", err)
				}
				if n != 0 {
					t.Fatalf("副作用面未登记失败 ⇒ 整批仍提交了（半截事务），实得 %d 行", n)
				}
			},
		},
		{
			name: "快照面",
			fail: func(ctx context.Context, tx *Tx) error {
				// 序号 0 ⇒ 落库前的拒绝（**不经** exec.run 的那条路径）。
				return tx.Snapshots().SaveIfNewer(ctx,
					checkpoint.Snapshot{RunID: "run-bad", Seq: 0})
			},
			write: func(ctx context.Context, tx *Tx) error {
				return tx.Snapshots().SaveIfNewer(ctx,
					checkpoint.Snapshot{RunID: "run-ok", Seq: 1})
			},
			assertVoided: func(t *testing.T, ctx context.Context, s *Store) {
				t.Helper()
				if _, err := s.Snapshots().Load(ctx, "run-ok"); !errors.Is(err, checkpoint.ErrNotFound) {
					t.Fatalf("快照面未登记失败 ⇒ 整批仍提交了（半截事务），实得 err=%v", err)
				}
			},
		},
		{
			name: "事件面",
			fail: func(ctx context.Context, tx *Tx) error {
				// 载荷含不可序列化的值 ⇒ 事件行写入失败。
				_, err := tx.Events().Insert(ctx, eventbus.Event{
					SessionID: "s-bad", Type: "t",
					Payload: map[string]any{"bad": func() {}},
				})
				return err
			},
			write: func(ctx context.Context, tx *Tx) error {
				_, err := tx.Events().Insert(ctx, eventbus.Event{SessionID: "s-ok", Type: "t"})
				return err
			},
			assertVoided: func(t *testing.T, ctx context.Context, s *Store) {
				t.Helper()
				wm, err := s.Events().CommittedWatermark(ctx)
				if err != nil {
					t.Fatalf("读水位线失败: %v", err)
				}
				if wm != 0 {
					t.Fatalf("事件面未登记失败 ⇒ 整批仍提交了（半截事务），水位线=%d", wm)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := appCtx(t)
			s := openStore(t)
			s.Board().Seed(taskboard.Task{ID: "t1", State: taskboard.StateOpen})

			err := s.Apply(ctx, func(tx *Tx) error {
				// 内层失败：错误被吞掉。
				_ = tc.fail(ctx, tx)
				// 继续写（这些写也必须随整批一起消失）。
				if werr := tc.write(ctx, tx); werr != nil {
					return werr
				}
				return nil
			})
			if err == nil {
				t.Fatalf("%s：内层失败被吞掉时 Apply 不得报成功（本批已作废）", tc.name)
			}
			tc.assertVoided(t, ctx, s)
		})
	}
}

// TestApplyRejectsNilCallback 空回调是编程错误，显式报错（不静默提交空批）。
func TestApplyRejectsNilCallback(t *testing.T) {
	ctx := appCtx(t)
	s := openStore(t)
	if err := s.Apply(ctx, nil); err == nil {
		t.Fatal("空回调必须报错（不得静默提交空批）")
	}
}
