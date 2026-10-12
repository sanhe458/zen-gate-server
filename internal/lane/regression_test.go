package lane

import (
	"context"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestNextSeqCriticalSection is the compile-coupled pin for the mint fix:
// nextSeq is the extracted check-reset-increment critical section, and it did
// not exist before the fix (the steps were inlined and racy), so this test
// cannot even build against the unfixed code. It also hammers the section
// concurrently — with the mutex removed, a lost increment or a handed-out
// sequence twice eventually surfaces here (the window is nanoseconds wide, so
// treat the concurrent half as a smoke guard, not a guarantee). Run with -race.
func TestNextSeqCriticalSection(t *testing.T) {
	// Sequential invariant: a new stamp restarts the sequence at 1, the
	// same stamp continues it, and no increment is ever lost.
	base := time.Now().UnixMilli()
	for stamp := int64(0); stamp < 4; stamp++ {
		first := nextSeq(base + stamp)
		if first != 1 {
			t.Fatalf("stamp %d: first sequence = %d, want 1", stamp, first)
		}
		for want := int64(2); want <= 50; want++ {
			if got := nextSeq(base + stamp); got != want {
				t.Fatalf("stamp %d: sequence = %d, want %d", stamp, got, want)
			}
		}
	}

	// Concurrent hammer: every caller for one stamp must observe a distinct
	// sequence, and the count must match exactly (no lost increments).
	const n = 16
	const per = 250 // n*per stays under the 12-bit seq field
	stamp := base + 100
	seqs := make([]int64, 0, n*per)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			local := make([]int64, 0, per)
			for j := 0; j < per; j++ {
				local = append(local, nextSeq(stamp))
			}
			mu.Lock()
			seqs = append(seqs, local...)
			mu.Unlock()
		}()
	}
	wg.Wait()
	seen := make(map[int64]bool, len(seqs))
	for _, v := range seqs {
		if seen[v] {
			t.Fatalf("sequence %d handed out twice for one millisecond", v)
		}
		seen[v] = true
	}
	if len(seen) != n*per {
		t.Fatalf("distinct sequences = %d, want %d (an increment was lost)", len(seen), n*per)
	}
}

// TestMintSessionIdConcurrentUnique is the end-to-end guard for the same fix
// through the public API: concurrent mints for one millisecond must decode to
// distinct sequences. The seq field is 12 bits wide, so the hammer stays
// under 4096 mints or the encoding itself wraps and the check false-alarms.
// Run with -race.
func TestMintSessionIdConcurrentUnique(t *testing.T) {
	now := time.Now().UnixMilli()
	const n = 16
	const per = 250
	ids := make([]string, 0, n*per)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			local := make([]string, 0, per)
			for j := 0; j < per; j++ {
				local = append(local, MintSessionId(now))
			}
			mu.Lock()
			ids = append(ids, local...)
			mu.Unlock()
		}()
	}
	wg.Wait()

	seen := make(map[uint64]bool, len(ids))
	for _, id := range ids {
		seq, ok := mintedSeqOf(id, now)
		if !ok {
			t.Fatalf("id %s does not carry the expected stamp", id)
		}
		if seen[seq] {
			t.Fatalf("sequence %d handed out twice for one millisecond (id %s)", seq, id)
		}
		seen[seq] = true
	}
	if len(seen) != n*per {
		t.Fatalf("distinct sequences = %d, want %d (a reset must not lose increments)", len(seen), n*per)
	}
}

// mintedSeqOf decodes the sequence counter out of a session id minted for
// nowMS: the first 12 hex chars are the low 48 bits of ^(nowMS<<12|seq&0xFFF)
// — the complement was taken over 64 bits before truncation, so the decode
// must complement inside the 48-bit window, and only the low 36 bits of the
// stamp survive.
func mintedSeqOf(id string, nowMS int64) (uint64, bool) {
	if !strings.HasPrefix(id, "ses_") || len(id) < 16 {
		return 0, false
	}
	raw, err := hex.DecodeString(id[4:16])
	if err != nil || len(raw) != 6 {
		return 0, false
	}
	var value uint64
	for _, b := range raw {
		value = value<<8 | uint64(b)
	}
	value = ^value & 0xFFFFFFFFFFFF
	if int64(value>>12) != nowMS&0xFFFFFFFFF {
		return 0, false
	}
	return value & 0xFFF, true
}

// TestDecoderMessagesStopReasonNotOverwritten pins the message_stop guard:
// Anthropic sends message_delta (with the real stop_reason) BEFORE
// message_stop, so an unconditional setFinish(FinishStop) on message_stop
// used to report a max_tokens truncation to the client as a clean finish.
func TestDecoderMessagesStopReasonNotOverwritten(t *testing.T) {
	dec := NewDecoder("messages", nil, func(Chunk) {})
	dec.Decode([]byte(`{"type":"content_block_start","index":0,"content_block":{"type":"text"}}`))
	dec.Decode([]byte(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial"}}`))
	dec.Decode([]byte(`{"type":"message_delta","delta":{"stop_reason":"max_tokens"}}`))
	dec.Decode([]byte(`{"type":"message_stop"}`))
	res := dec.Finish()
	if res.Finish != FinishMaxTokens {
		t.Fatalf("truncated turn must stay max-tokens, got %s", res.Finish)
	}

	// A stream that never carried a reason still defaults to stop.
	dec2 := NewDecoder("messages", nil, func(Chunk) {})
	dec2.Decode([]byte(`{"type":"message_stop"}`))
	if res2 := dec2.Finish(); res2.Finish != FinishStop {
		t.Fatalf("reason-less stream must default to stop, got %s", res2.Finish)
	}
}

// TestDecoderChatToolContinuationWithoutID pins the deterministic fallback:
// a bare argument fragment (no index, no id) must attach to the most recently
// opened tool block. The old code iterated the blocks map — random order — so
// with several tools open the fragment could land on any of them.
func TestDecoderChatToolContinuationWithoutID(t *testing.T) {
	const open = 8
	argsByIdx := map[int]string{}
	dec := NewDecoder("chat", nil, func(c Chunk) {
		if c.Kind == ChunkToolCallDelta {
			argsByIdx[c.Index] += c.Delta
		}
	})
	// eight parallel calls, each opened by id (no index)
	for i := 0; i < open; i++ {
		frame := `{"choices":[{"delta":{"tool_calls":[{"id":"call_` +
			string(rune('a'+i)) + `","function":{"name":"bash","arguments":"{\"x` +
			string(rune('0'+i)) + `\":"}}]}}]}`
		dec.Decode([]byte(frame))
	}
	// bare continuations, no id and no index: every one belongs to the LAST
	// opened tool block (highest index).
	for i := 0; i < open; i++ {
		dec.Decode([]byte(`{"choices":[{"delta":{"tool_calls":[{"function":{"arguments":"1}"}}]}}]}`))
	}
	last := open - 1
	want := `{"x` + string(rune('0'+last)) + `":` + strings.Repeat("1}", open)
	if got := argsByIdx[last]; got != want {
		t.Fatalf("bare continuations must all attach to block %d: got %q, want %q", last, got, want)
	}
	for i := 0; i < last; i++ {
		if got := argsByIdx[i]; strings.Contains(got, "1}") {
			t.Fatalf("a bare continuation landed on block %d (%q), not the most recent one", i, got)
		}
	}
}

// TestSetProxyConcurrentWithRequests is the race-detector guard for the
// transport swap: SetProxy used to write laneHTTP.Transport while Do() read
// it. The transport is now stable and the egress decider swaps atomically.
// Run with -race.
func TestSetProxyConcurrentWithRequests(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer up.Close()
	old := UpstreamBase
	UpstreamBase = up.URL
	defer func() { UpstreamBase = old }()

	var wg sync.WaitGroup
	stop := make(chan struct{})
	// config writer: hot-swap the egress while traffic flows
	wg.Add(1)
	go func() {
		defer wg.Done()
		modes := []struct{ mode, url string }{
			{"direct", ""}, {"env", ""}, {"custom", up.URL}, {"pool", ""}, {"system", ""},
		}
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			m := modes[i%len(modes)]
			SetProxy(m.mode, m.url)
			SetActiveProxy(nil)
		}
	}()
	// readers: GetJSON shares the same transport
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var out map[string]any
			for j := 0; j < 50; j++ {
				_ = GetJSON(context.Background(), "/v1/models", "ses_x", "msg_x", &out)
			}
		}()
	}
	// small sleep to let the writers and readers overlap, then stop
	time.Sleep(100 * time.Millisecond)
	close(stop)
	wg.Wait()
	// leave the package in the default state for other tests
	SetProxy("env", "")
}

// TestDetectEgressHangingServerTimesOut pins the per-attempt timeout: the
// shared client has no overall timeout, so a hanging echo service used to
// block the probe round forever. Run with -race.
func TestDetectEgressHangingServerTimesOut(t *testing.T) {
	old := egressAttemptTimeout
	egressAttemptTimeout = 150 * time.Millisecond
	defer func() { egressAttemptTimeout = old }()

	// A server that accepts and then never answers, for every echo service.
	hung := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer hung.Close()

	client := &http.Client{} // no timeout of its own, like laneHTTP
	done := make(chan Egress, 1)
	go func() { done <- DetectEgressVia(context.Background(), client) }()
	select {
	case e := <-done:
		if e.IP != "" {
			t.Fatalf("a hanging echo service must not yield an egress, got %+v", e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("DetectEgressVia blocked on a hanging server: per-attempt timeout missing")
	}
}

// TestThrottleSnapshotsDoNotAliasLedger pins the deep-copy fix: values that
// leave the book (fire callbacks, Note, All) used to share the Episodes
// backing array with the ledger, which mutates those elements in place
// (closing open episodes). Run with -race.
func TestThrottleSnapshotsDoNotAliasLedger(t *testing.T) {
	b := NewThrottleBook()
	var fired []ThrottleNote
	var mu sync.Mutex
	b.OnUpdate = func(model string, note ThrottleNote) {
		mu.Lock()
		fired = append(fired, note)
		mu.Unlock()
	}
	b.MarkThrottled("m", 60)
	b.MarkOK("m")

	note := b.Note("m")
	if len(note.Episodes) == 0 {
		t.Fatal("episode history must survive the throttle cycle")
	}
	// Mutating a returned snapshot must not reach back into the ledger.
	note.Episodes[0].Start = 1
	note.Episodes[0].End = 2
	if again := b.Note("m"); again.Episodes[0].Start == 1 || again.Episodes[0].End == 2 {
		t.Fatal("Note() returned a snapshot aliasing the ledger's Episodes array")
	}
	all := b.All()
	all["m"].Episodes[0].Start = 3
	if again := b.All(); again["m"].Episodes[0].Start == 3 {
		t.Fatal("All() returned a snapshot aliasing the ledger's Episodes array")
	}

	// Concurrent readers while the ledger mutates: the race detector is the
	// assertion here.
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				_ = b.All()
				_ = b.Note("m")
				_ = b.RecoveryETA("m")
				_ = b.Throttled("m")
			}
		}()
	}
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				b.MarkThrottled("m", 0)
				b.MarkOK("m")
			}
		}()
	}
	wg.Wait()
}

// TestSetProxyCustomDeciderReachesTransport checks the rebuilt egress path:
// a custom proxy URL must be the one the per-request decider returns, and the
// decider must be swappable while the transport itself never changes.
func TestSetProxyCustomDeciderReachesTransport(t *testing.T) {
	SetProxy("custom", "127.0.0.1:9999")
	defer SetProxy("env", "")

	req, err := http.NewRequest(http.MethodGet, "http://example.invalid/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	f := proxyDecider.Load()
	if f == nil {
		t.Fatal("decider must be installed")
	}
	u, err := (*f)(req)
	if err != nil {
		t.Fatalf("custom decider errored: %v", err)
	}
	if u == nil || u.Host != "127.0.0.1:9999" || u.Scheme != "http" {
		t.Fatalf("custom decider returned %v, want http://127.0.0.1:9999", u)
	}

	// An unparsable URL falls back to the environment decider, matching the
	// old transport-rebuild behaviour. (Compared by function pointer: the
	// env result itself depends on cached process-wide state.)
	SetProxy("custom", "://nope")
	f2 := proxyDecider.Load()
	if f2 == nil || reflect.ValueOf(*f2).Pointer() != reflect.ValueOf(http.ProxyFromEnvironment).Pointer() {
		t.Fatal("unparsable custom URL must fall back to the environment decider")
	}
}

// TestLaneSettingsConcurrentWithListings is the race-detector guard for the
// exposeRegion fix: the admin settings handler writes lane settings while
// /v1 listings read them. ServableModels read l.exposeRegion outside the
// mutex, which the race detector flags the moment a settings save overlaps a
// model listing (found by stress-running the real server with -race).
// Run with -race.
func TestLaneSettingsConcurrentWithListings(t *testing.T) {
	l := NewLane()
	// Seed a catalog with one region-blocked and one healthy model.
	l.mu.Lock()
	l.catalog = []ModelInfo{
		{ID: "healthy-free", Name: "healthy", Wire: "chat"},
		{ID: "blocked-free", Name: "blocked", Wire: "chat"},
	}
	l.availability = map[string]ProbeResult{
		"healthy-free": {Model: "healthy-free", State: StateAvailable},
		"blocked-free": {Model: "blocked-free", State: StateRegionBlock},
	}
	l.mu.Unlock()

	// Sequential behaviour: the flag decides whether the blocked id shows.
	l.SetExposeRegion(false)
	for _, m := range l.ServableModels() {
		if m.ID == "blocked-free" {
			t.Fatal("region-blocked model listed while exposeRegion is off")
		}
	}
	l.SetExposeRegion(true)
	found := false
	for _, m := range l.ServableModels() {
		if m.ID == "blocked-free" {
			found = true
		}
	}
	if !found {
		t.Fatal("region-blocked model missing from listing while exposeRegion is on")
	}

	// Concurrent settings writes vs listings: the race detector is the
	// assertion (the old lock-free read tripped it within seconds).
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			l.SetExposeRegion(i%2 == 0)
			l.SetDefaultMaxTokens(1024 + i%1024)
			l.SetFailover(i%2 == 0, 1+i%5)
		}
	}()
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = l.ServableModels()
				_, _, _ = l.Snapshot()
			}
		}()
	}
	time.Sleep(150 * time.Millisecond)
	close(stop)
	wg.Wait()
}
