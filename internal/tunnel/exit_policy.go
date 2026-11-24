package tunnel

import "github.com/urnetwork/connect"

// singleExitSettings enables connect's Fixed IP (sticky-exit) policy. A fixed
// profile is essential: setting only the window sizes leaves auto mode free to
// route different traffic classes through different quality/speed windows.
func singleExitSettings() *connect.MultiClientSettings {
	settings := connect.DefaultMultiClientSettings()
	settings.DefaultPerformanceProfile = &connect.PerformanceProfile{
		WindowType: connect.WindowTypeQuality,
		WindowSize: connect.WindowSizeSettings{
			WindowSizeMin:       1,
			WindowSizeMax:       1,
			WindowSizeHardMax:   1,
			FixedWindowSize:     1,
			KeepHealthiestCount: 1,
		},
	}
	settings.StrictWindowSizeHardMax = true
	settings.StandingReserve = false
	settings.LimitedWindowSizeFraction = 0
	settings.MaxClientLifetime = 0
	// Test the random draw on its own. Parallel evaluation would bias the
	// winner back toward the same fastest provider on every machine.
	settings.EvaluationPoolMultiple = 1
	settings.WindowExpandBlockCount = 1
	// "ipv4" accepts every provider (including dualstack) without reserving
	// an IPv6 slot. Otherwise connect may replace a healthy IPv4 exit merely
	// because a dualstack provider becomes available later.
	settings.NormalWindowSelectors = connect.ProviderSelectors{
		Ip:       connect.ProviderIpCapabilityIpv4,
		Protocol: connect.ProviderProtocolCapabilityDualproto,
	}
	// Health checks, blackhole/stall detection and refill remain enabled.
	// The sticky policy retries the original provider after transport loss,
	// and discovers a replacement when that provider is gone or fails health.
	return settings
}
