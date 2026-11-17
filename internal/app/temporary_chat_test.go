package app

import "testing"

// The deprecated multi_turn setting no longer overrides temporary chats.
func TestTemporaryChatEligible(t *testing.T) {
	old := rtCfg()
	defer func() { rtMu.Lock(); rtVal = old; rtMu.Unlock() }()

	cases := []struct {
		temp, multi, want bool
	}{
		{false, false, false}, // 都关
		{true, false, true},   // 只开临时 → 生效
		{true, true, true},    // 已停用的兼容字段不再覆盖临时对话
		{false, true, false},  // 只开多轮
	}
	for _, c := range cases {
		rtMu.Lock()
		rtVal.TemporaryChat = c.temp
		rtVal.MultiTurn = c.multi
		rtMu.Unlock()
		if got := temporaryChatEligible(); got != c.want {
			t.Errorf("temp=%v multi=%v => %v, 期望 %v", c.temp, c.multi, got, c.want)
		}
	}
}
