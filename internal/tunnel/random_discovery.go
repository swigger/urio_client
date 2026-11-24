package tunnel

import (
	"context"
	"crypto/rand"
	"fmt"
	"log/slog"
	"math/big"

	"github.com/urnetwork/connect"
)

// Fetch more than the SDK's small, quality-ranked evaluation batch. The API
// has quality/speed modes, but no random rank mode. Draw locally from the
// eligible response before the SDK can rank or race candidates again.
const randomExitPoolSize = 256

type randomDiscovery struct {
	base      connect.NetworkProviderDiscovery
	pickIndex func(int) (int, error) // nil uses independent OS randomness
}

func (d *randomDiscovery) FindProviders2(ctx context.Context, token string, args *connect.FindProviders2Args) (*connect.FindProviders2Result, error) {
	// Sticky reconnect names its original provider. Keep that path intact,
	// including an empty answer which makes the SDK discover a replacement.
	named := len(args.Specs) > 0
	for _, spec := range args.Specs {
		if spec == nil || spec.ClientId == nil {
			named = false
			break
		}
	}
	if named {
		return d.base.FindProviders2(ctx, token, args)
	}

	request := *args
	request.Count = randomExitPoolSize
	result, err := d.base.FindProviders2(ctx, token, &request)
	if err != nil {
		return nil, err
	}
	if result == nil {
		return nil, fmt.Errorf("provider discovery returned no result")
	}
	// Do not give a duplicate provider extra lottery tickets. Enforce the
	// caller's failure exclusions even if an older server ignores them.
	excluded := map[connect.Id]bool{}
	for _, id := range args.ExcludeClientIds {
		excluded[id] = true
	}
	seen := map[connect.Id]bool{}
	pool := make([]*connect.FindProvidersProvider, 0, len(result.Providers))
	for _, p := range result.Providers {
		if p == nil || seen[p.ClientId] || excluded[p.ClientId] || p.ClientId == (connect.Id{}) {
			continue
		}
		blocked := false
		for _, hop := range p.IntermediaryIds {
			if excluded[hop] {
				blocked = true
				break
			}
		}
		for _, path := range args.ExcludeDestinations {
			// The client uses a single exit, so exclude its provider even if
			// discovery offers a different intermediary route to the same exit.
			if len(path) > 0 && path[len(path)-1] == p.ClientId {
				blocked = true
				break
			}
		}
		if blocked {
			continue
		}
		seen[p.ClientId] = true
		pool = append(pool, p)
	}
	// Preserve the requested protocol's preference: a TCP-only overflow
	// candidate must not displace an available TCP/UDP provider by lottery.
	if args.Select != nil && result.Select != nil && args.Select.Protocol == connect.ProviderProtocolCapabilityDualproto {
		dual := make([]*connect.FindProvidersProvider, 0, len(pool))
		for _, p := range pool {
			if p.Capabilities != nil && p.Capabilities.Protocol == connect.ProviderProtocolCapabilityDualproto {
				dual = append(dual, p)
			}
		}
		if len(dual) > 0 {
			pool = dual
		}
	}
	answer := *result
	answer.Providers = nil
	if len(pool) == 0 {
		return &answer, nil
	}
	pick := d.pickIndex
	if pick == nil {
		pick = randomIndex
	}
	i, err := pick(len(pool))
	if err != nil {
		return nil, fmt.Errorf("random exit selection: %w", err)
	}
	if i < 0 || i >= len(pool) {
		return nil, fmt.Errorf("random exit selection returned invalid index")
	}
	answer.Providers = []*connect.FindProvidersProvider{pool[i]}
	slog.Info("[EXIT] randomly selected candidate", "provider", pool[i].ClientId, "candidates", len(pool))
	return &answer, nil
}

func randomIndex(n int) (int, error) {
	i, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
	if err != nil {
		return 0, err
	}
	return int(i.Int64()), nil
}
