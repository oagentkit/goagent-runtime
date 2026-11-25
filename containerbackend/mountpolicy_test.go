// mountpolicy_test.go —— 挂载白名单（issue #187）的回归面，与判据同文件放置。

package containerbackend

import (
	"context"
	"net/http/httptest"
	"testing"

	kerrors "github.com/oekit/pkg/errors"

	"github.com/oagentkit/goagent-kernel/backend"
)

// TestDockerMountPolicy 挂载白名单的回归面（issue #187）。
//
// 反面（攻击形态必须被创建阶段拦下，且不触达引擎）与正面（白名单内照常）
// 两面都钉：缺了反面，策略形同虚设；缺了正面，宿主会把白名单当摆设拆掉。
func TestDockerMountPolicy(t *testing.T) {
	denied := func(t *testing.T, d *Docker, f *fakeEngine, m backend.Mount) {
		t.Helper()
		res, err := d.Exec(context.Background(), backend.Job{Image: "img", Mounts: []backend.Mount{m}})
		if err == nil {
			t.Fatalf("挂载 %+v 必须被拒绝（res=%+v）", m, res)
		}
		if !kerrors.Is(err, ErrMountDenied) {
			t.Fatalf("期望 ErrMountDenied，得到 %v", err)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if len(f.createReqs) != 0 {
			t.Fatalf("策略拒绝必须先于引擎调用（createReqs=%d）", len(f.createReqs))
		}
	}

	// ── 反面：攻击形态 ──
	t.Run("缺省策略（未装配白名单）：一切挂载拒绝", func(t *testing.T) {
		f := &fakeEngine{}
		srv := httptest.NewServer(f.handler())
		t.Cleanup(srv.Close)
		d := NewDocker(WithEndpoint(srv.URL)) // 不装配策略 = 空白名单
		denied(t, d, f, backend.Mount{Source: "/src", Target: "/dst", ReadOnly: true})
	})

	t.Run("挂 docker.sock：拒绝（断网隔离可被它整个绕开）", func(t *testing.T) {
		f := &fakeEngine{}
		srv := httptest.NewServer(f.handler())
		t.Cleanup(srv.Close)
		d := NewDocker(WithEndpoint(srv.URL), WithMountPolicy(MountPolicy{AllowedSources: []string{"/src"}}))
		denied(t, d, f, backend.Mount{Source: "/var/run/docker.sock", Target: "/var/run/docker.sock", ReadOnly: true})
	})

	t.Run("白名单外目录（/etc）：拒绝", func(t *testing.T) {
		f := &fakeEngine{}
		srv := httptest.NewServer(f.handler())
		t.Cleanup(srv.Close)
		d := NewDocker(WithEndpoint(srv.URL), WithMountPolicy(MountPolicy{AllowedSources: []string{"/src"}}))
		denied(t, d, f, backend.Mount{Source: "/etc", Target: "/etc", ReadOnly: true})
	})

	t.Run("读写挂载而策略未放开：拒绝（缺省只读）", func(t *testing.T) {
		f := &fakeEngine{}
		srv := httptest.NewServer(f.handler())
		t.Cleanup(srv.Close)
		d := NewDocker(WithEndpoint(srv.URL), WithMountPolicy(MountPolicy{AllowedSources: []string{"/src"}}))
		denied(t, d, f, backend.Mount{Source: "/src", Target: "/dst"})
	})

	t.Run("Target 注入 bind 选项（:rw）：拒绝", func(t *testing.T) {
		f := &fakeEngine{}
		srv := httptest.NewServer(f.handler())
		t.Cleanup(srv.Close)
		d := NewDocker(WithEndpoint(srv.URL),
			WithMountPolicy(MountPolicy{AllowedSources: []string{"/src"}, AllowWritable: true}))
		denied(t, d, f, backend.Mount{Source: "/src", Target: "/dst:ro,rw"})
	})

	t.Run("相对来源（命名卷形态，绕开宿主路径控制）：拒绝", func(t *testing.T) {
		f := &fakeEngine{}
		srv := httptest.NewServer(f.handler())
		t.Cleanup(srv.Close)
		d := NewDocker(WithEndpoint(srv.URL),
			WithMountPolicy(MountPolicy{AllowedSources: []string{"/src"}, AllowWritable: true}))
		denied(t, d, f, backend.Mount{Source: "mydata", Target: "/dst", ReadOnly: true})
	})

	t.Run("前缀串接不在子树内（/secret2 ≠ /secret 子树）：拒绝", func(t *testing.T) {
		f := &fakeEngine{}
		srv := httptest.NewServer(f.handler())
		t.Cleanup(srv.Close)
		d := NewDocker(WithEndpoint(srv.URL), WithMountPolicy(MountPolicy{AllowedSources: []string{"/secret"}}))
		denied(t, d, f, backend.Mount{Source: "/secret2", Target: "/dst", ReadOnly: true})
	})

	// ── 正面：合法形态照常 ──
	t.Run("白名单目录 + ReadOnly：通过且 bind 落 :ro", func(t *testing.T) {
		f := &fakeEngine{}
		d := newFake(t, f) // newFake 装配 /src 白名单
		res, err := d.Exec(context.Background(), backend.Job{
			Image:  "img",
			Mounts: []backend.Mount{{Source: "/src", Target: "/dst", ReadOnly: true}},
		})
		if err != nil || !res.OK {
			t.Fatalf("白名单内只读挂载必须通过：err=%v res=%+v", err, res)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if len(f.createReqs) != 1 || len(f.createReqs[0].HostConfig.Binds) != 1 ||
			f.createReqs[0].HostConfig.Binds[0] != "/src:/dst:ro" {
			t.Fatalf("bind 声明漂移：%+v", f.createReqs)
		}
	})

	t.Run("白名单子树内的子目录：通过", func(t *testing.T) {
		f := &fakeEngine{}
		srv := httptest.NewServer(f.handler())
		t.Cleanup(srv.Close)
		d := NewDocker(WithEndpoint(srv.URL), WithMountPolicy(MountPolicy{AllowedSources: []string{"/src"}}))
		if _, err := d.Exec(context.Background(), backend.Job{
			Image:  "img",
			Mounts: []backend.Mount{{Source: "/src/work/asset", Target: "/in", ReadOnly: true}},
		}); err != nil {
			t.Fatalf("子树内路径必须通过：%v", err)
		}
	})

	t.Run("AllowWritable 放开后读写挂载：通过且引擎侧无 :ro", func(t *testing.T) {
		f := &fakeEngine{}
		srv := httptest.NewServer(f.handler())
		t.Cleanup(srv.Close)
		d := NewDocker(WithEndpoint(srv.URL),
			WithMountPolicy(MountPolicy{AllowedSources: []string{"/src"}, AllowWritable: true}))
		if _, err := d.Exec(context.Background(), backend.Job{
			Image:  "img",
			Mounts: []backend.Mount{{Source: "/src", Target: "/dst"}},
		}); err != nil {
			t.Fatalf("放开读写后必须通过：%v", err)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if len(f.createReqs) != 1 || f.createReqs[0].HostConfig.Binds[0] != "/src:/dst" {
			t.Fatalf("读写 bind 声明漂移：%+v", f.createReqs)
		}
	})
}
