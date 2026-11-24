package tunnel

import (
	"testing"

	"github.com/urnetwork/connect"
)

func TestDefaultExitPolicyKeepsOneProviderUntilFailure(t *testing.T) {
	s := singleExitSettings()
	if err := s.DefaultPerformanceProfile.Validate(); err != nil {
		t.Fatal(err)
	}
	kind, size, fixed := s.DefaultPerformanceProfile.FixedWindow()
	if !fixed || kind != connect.WindowTypeQuality || size.FixedWindowSize != 1 {
		t.Fatal("all protocols must share the upstream Fixed IP window")
	}
	if size.WindowSizeMin != 1 || size.WindowSizeMax != 1 || size.WindowSizeHardMax != 1 || !s.StrictWindowSizeHardMax {
		t.Fatal("a second selectable provider could be admitted")
	}
	if s.StandingReserve || s.LimitedWindowSizeFraction != 0 || s.MaxClientLifetime != 0 {
		t.Fatal("spare exits or scheduled rotation could change a healthy exit")
	}
	if s.EvaluationPoolMultiple != 1 || s.WindowExpandBlockCount != 1 {
		t.Fatal("random candidates must be evaluated one at a time, without a fastest-wins race")
	}
	if s.NormalWindowSelectors.Ip != connect.ProviderIpCapabilityIpv4 || s.NormalWindowSelectors.Protocol != connect.ProviderProtocolCapabilityDualproto {
		t.Fatal("capability upgrades must not replace a healthy exit; prefer TCP/UDP-capable providers initially")
	}
	if s.StatsWindowMaxUnhealthyDuration <= 0 || s.SendStallTimeout <= 0 {
		t.Fatal("failure detection must remain enabled")
	}
}
