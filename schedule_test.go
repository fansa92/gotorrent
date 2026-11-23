package main

import "testing"

func TestParseScheduleRule(t *testing.T) {
	cases := []struct {
		in       string
		wantDown int64
		wantUp   int64
		overMid  bool
		ok       bool
	}{
		{"09:00-23:00,1M,256K", 1 << 20, 256 << 10, false, true},
		{"23:00-07:00,0,0", 0, 0, true, true},
		{"00:00-23:59,5M,1M", 5 << 20, 1 << 20, false, true},
	}
	for _, c := range cases {
		r, err := parseSchedRule(c.in)
		if c.ok && err != nil {
			t.Errorf("%q: 不期望的错误 %v", c.in, err)
			continue
		}
		if !c.ok && err == nil {
			t.Errorf("%q: 期望错误但没有", c.in)
			continue
		}
		if !c.ok {
			continue
		}
		if r.down != c.wantDown || r.up != c.wantUp {
			t.Errorf("%q: down=%d(期望 %d) up=%d(期望 %d)", c.in, r.down, c.wantDown, r.up, c.wantUp)
		}
		if r.overMid != c.overMid {
			t.Errorf("%q: overMid=%v(期望 %v)", c.in, r.overMid, c.overMid)
		}
	}
}

func TestScheduleMatch(t *testing.T) {
	r, err := parseSchedRule("09:00-23:00,1M,256K")
	if err != nil {
		t.Fatal(err)
	}
	// 12:00 应命中,01:00 不命中
	if !r.match(12 * 3600 * 1e9) {
		t.Error("12:00 应命中 09:00-23:00")
	}
	if r.match(1 * 3600 * 1e9) {
		t.Error("01:00 不应命中 09:00-23:00")
	}

	// 跨零点规则
	r2, _ := parseSchedRule("23:00-07:00,0,0")
	if !r2.match(23*3600*1e9 + 30*1e9) {
		t.Error("23:00:30 应命中跨零点规则")
	}
	if !r2.match(3 * 3600 * 1e9) {
		t.Error("03:00 应命中跨零点规则")
	}
	if r2.match(12 * 3600 * 1e9) {
		t.Error("12:00 不应命中跨零点规则")
	}
}
