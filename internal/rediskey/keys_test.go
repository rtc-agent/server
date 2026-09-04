package rediskey

import "testing"

func TestWorkerKeyConstructors(t *testing.T) {
	tests := []struct {
		name string
		got  string
		want string
	}{
		{"WorkerInfo", WorkerInfo("w1"), "worker:w1"},
		{"WorkerSessions", WorkerSessions("w1"), "worker:w1:sessions"},
		{"WorkerQueue", WorkerQueue("w1"), "worker:w1:queue"},
		{"SessionAffinity", SessionAffinity(), "session:affinity"},
		{"WorkersActive", WorkersActive(), "workers:active"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Errorf("got %q, want %q", tt.got, tt.want)
			}
		})
	}
}

func TestWorkerPrefixConstants(t *testing.T) {
	if PrefixWorkerInfo != "worker:" {
		t.Errorf("PrefixWorkerInfo = %q, want %q", PrefixWorkerInfo, "worker:")
	}
	if PrefixWorkerSessions != "worker:" {
		t.Errorf("PrefixWorkerSessions = %q, want %q", PrefixWorkerSessions, "worker:")
	}
	if PrefixWorkerQueue != "worker:" {
		t.Errorf("PrefixWorkerQueue = %q, want %q", PrefixWorkerQueue, "worker:")
	}
	if SessionAffinityKey != "session:affinity" {
		t.Errorf("SessionAffinityKey = %q, want %q", SessionAffinityKey, "session:affinity")
	}
	if WorkersActiveKey != "workers:active" {
		t.Errorf("WorkersActiveKey = %q, want %q", WorkersActiveKey, "workers:active")
	}
}

func TestMessageStream_Key(t *testing.T) {
	tests := []struct {
		name    string
		msgID   string
		wantKey string
	}{
		{"simple ID", "msg-001", "message:stream:msg-001"},
		{"UUID", "550e8400-e29b-41d4-a716-446655440000", "message:stream:550e8400-e29b-41d4-a716-446655440000"},
		{"empty string", "", "message:stream:"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MessageStream(tt.msgID)
			if got != tt.wantKey {
				t.Errorf("MessageStream(%q) = %q, want %q", tt.msgID, got, tt.wantKey)
			}
		})
	}
}

func TestMessageStream_PrefixConstant(t *testing.T) {
	if PrefixMessageStream != "message:stream:" {
		t.Errorf("PrefixMessageStream = %q, want %q", PrefixMessageStream, "message:stream:")
	}
}
