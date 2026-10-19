package workspace

import "testing"

// TestLimitsClampToHardCap 断言配置只能把上限调**小**，不能调大（D20）。
func TestLimitsClampToHardCap(t *testing.T) {
	cases := []struct {
		name                              string
		in                                Options
		wantActive, wantDepth, wantFanout int
	}{
		{"零值取硬上限", Options{}, HardMaxActive, HardMaxDepth, HardMaxFanout},
		{"调小生效", Options{MaxActive: 8, MaxDepth: 4, MaxFanout: 2}, 8, 4, 2},
		{"超过硬上限被钳回", Options{MaxActive: 999, MaxDepth: 999, MaxFanout: 999},
			HardMaxActive, HardMaxDepth, HardMaxFanout},
		{"负数取硬上限", Options{MaxActive: -1, MaxDepth: -5, MaxFanout: -3},
			HardMaxActive, HardMaxDepth, HardMaxFanout},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := &Manager{opt: c.in}
			a, d, f := m.limits()
			if a != c.wantActive || d != c.wantDepth || f != c.wantFanout {
				t.Fatalf("limits() = (%d,%d,%d)，期望 (%d,%d,%d)",
					a, d, f, c.wantActive, c.wantDepth, c.wantFanout)
			}
		})
	}
}

// TestMaxActiveIsHardCappedAt64 单独钉住 D20：max_active 的硬上限就是 64。
func TestMaxActiveIsHardCappedAt64(t *testing.T) {
	if HardMaxActive != 64 {
		t.Fatalf("HardMaxActive = %d，D20 规定为 64", HardMaxActive)
	}
	m := &Manager{opt: Options{MaxActive: 1000}}
	if a, _, _ := m.limits(); a != 64 {
		t.Fatalf("配置 1000 被放行成 %d —— 硬上限失效", a)
	}
}
