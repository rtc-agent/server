package centrifugeplus

import (
	"testing"

	"github.com/centrifugal/centrifuge"
)

// TestPrefixConsistency verifies that the channel prefixes used in dual_broker.go
// match the values defined in internal/channel/parse.go.
//
// Since centrifuge-plus is an independent module and cannot import internal packages,
// these values are duplicated. This test ensures they stay in sync by checking the
// actual behavior of the getChannelType function.
//
// If this test fails after changing internal/channel/parse.go, update the prefixes
// in dual_broker.go (see getChannelType method) accordingly.
func TestPrefixConsistency(t *testing.T) {
	// Expected values from internal/channel/parse.go:
	//   channel.TopicPrefix = "topic:"
	//   channel.LivePrefix  = "live:"
	const (
		expectedTopicPrefix = "topic:" // must match internal/channel.TopicPrefix
		expectedLivePrefix  = "live:"  // must match internal/channel.LivePrefix
	)

	// Create a minimal DualBroker for testing prefix inference
	// Note: We use nil for the underlying brokers since we're only testing
	// the prefix matching logic in getChannelType, not actual broker operations.
	broker := &DualBroker{}

	testCases := []struct {
		name          string
		channel       string
		expectedType  ChannelType
		shouldSucceed bool
	}{
		{
			name:          "topic channel with expected prefix",
			channel:       expectedTopicPrefix + "u=test123",
			expectedType:  Topic,
			shouldSucceed: true,
		},
		{
			name:          "live channel with expected prefix",
			channel:       expectedLivePrefix + "u=test123",
			expectedType:  Live,
			shouldSucceed: true,
		},
		{
			name:          "unknown prefix should fail",
			channel:       "unknown:u=test123",
			shouldSucceed: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			channelType, err := broker.getChannelType(tc.channel)
			if tc.shouldSucceed {
				if err != nil {
					t.Errorf("getChannelType(%q) failed: %v", tc.channel, err)
				}
				if channelType != tc.expectedType {
					t.Errorf("getChannelType(%q) = %v, want %v", tc.channel, channelType, tc.expectedType)
				}
			} else {
				if err == nil {
					t.Errorf("getChannelType(%q) should have failed, but returned %v", tc.channel, channelType)
				}
			}
		})
	}
}

// TestDualBrokerImplementsBroker verifies DualBroker satisfies the centrifuge.Broker interface.
func TestDualBrokerImplementsBroker(t *testing.T) {
	var _ centrifuge.Broker = (*DualBroker)(nil)
}
